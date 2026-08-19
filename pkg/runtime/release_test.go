package runtime

import (
	"context"
	"encoding/hex"
	"strings"
	"sync"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/punish"
)

// A release needs the bond on the chain
// and somewhere to pay it.
func TestAReleaseNeedsTheBondAndAPayout(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := seatTwo(t, fake, rt)

	err := rt.Release(context.Background(), sid)
	if err == nil {
		t.Fatal("released a bond that is not on the chain")
	}
	if !strings.Contains(err.Error(), "not on the chain") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// readyToRelease seats two, sets a payout for both, and puts this seat's table
// bond on the chain - the state a release needs.
func readyToRelease(t *testing.T, rt *Runtime, fake *bridgetest.Bridge) (string, *peer) {
	t.Helper()
	sid, them := seatTwo(t, fake, rt)
	pay, err := payScriptFor(payTo(t), chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("payout: %v", err)
	}
	for seat := range mustSeats(t, rt, sid) {
		if err := rt.SetPayoutFor(sid, seat, pay); err != nil {
			t.Fatalf("payout for seat %d: %v", seat, err)
		}
	}
	quickPoll(t)
	if err := rt.FundTableBond(context.Background(), sid); err != nil {
		t.Fatalf("fund the table bond: %v", err)
	}
	return sid, them
}

// The whole cooperative path: this seat signs and sends nothing, the other
// signs, and the bond goes home.
func TestATableBondBothSeatsSignGoesHome(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := readyToRelease(t, rt, fake)

	if err := rt.Release(context.Background(), sid); err != nil {
		t.Fatalf("release: %v", err)
	}
	rt.mu.Lock()
	mine, _ := rt.tables[sid].form.OurSeat()
	rel := rt.tables[sid].releases[mine]
	sent := rel.done
	rt.mu.Unlock()
	if sent {
		t.Fatal("a release went out with only one signature")
	}

	// The other seat signs the same transaction.
	sig, err := escrow.SignBondSpend(rel.tx, rel.draft.Bond, them.creds.Session)
	if err != nil {
		t.Fatalf("their signature: %v", err)
	}
	theirSeat, _ := them.form.OurSeat()
	seats, _ := them.form.Seats()
	// Named the way a real peer names it: whose bond, and the transaction
	// itself, so the receiver rebuilds what it is being asked to agree with
	// rather than believing it.
	raw, err := rel.tx.Bytes()
	if err != nil {
		t.Fatalf("serialise: %v", err)
	}
	body := schema.Release{
		Seat: ourSeatOf(t, rt, sid), Tx: hex.EncodeToString(raw),
		Signer: hex.EncodeToString(seats[theirSeat]), Sig: hex.EncodeToString(sig),
	}
	if err := rt.adoptRelease(context.Background(), sid, body); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	sent = ourRelease(rt, sid).done
	if !sent {
		t.Fatal("a fully signed release was not sent")
	}
}

// Withholding a signature does not strand the money: the same bond has a
// backstop branch its owner spends alone once the lock matures.
func TestAReleaseIsNotSentShortOfASignature(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := readyToRelease(t, rt, fake)

	if err := rt.Release(context.Background(), sid); err != nil {
		t.Fatalf("release: %v", err)
	}
	done := ourRelease(rt, sid).done
	if done {
		t.Fatal("a release short of a signature was sent")
	}

	// And the owner can still take it alone at the lock.
	if err := rt.BackstopRelease(context.Background(), sid); err != nil {
		t.Fatalf("backstop: %v", err)
	}
	done = ourRelease(rt, sid).done
	if !done {
		t.Fatal("the backstop did not send")
	}
}

func TestAdoptingAReleaseRefusesTheObviouslyWrong(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := seatTwo(t, fake, rt)
	for _, tc := range []struct {
		name  string
		match string
		body  schema.Release
	}{
		{"a table this game is not at", "no-such-table", schema.Release{}},
		{"a signer who is not at the table", sid, schema.Release{Signer: "aabb"}},
		{"nothing proposed to agree with", sid, schema.Release{
			Signer: signerAt(t, rt, sid), Sig: "aabb",
		}},
	} {
		if err := rt.adoptRelease(context.Background(), tc.match, tc.body); err == nil {
			t.Errorf("%s: adopted", tc.name)
		}
	}
}

// A release already sent is not sent twice.
func TestAReleaseAlreadySentIsNotSentAgain(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := seatTwo(t, fake, rt)
	rt.mu.Lock()
	tbl := rt.tables[sid]
	mine, _ := tbl.form.OurSeat()
	tbl.releases = map[uint32]*release{mine: {seat: mine, done: true}}
	rt.mu.Unlock()
	if err := rt.completeRelease(context.Background(), tbl, mine); err != nil {
		t.Fatalf("completing a finished release: %v", err)
	}
}

func signerAt(t *testing.T, rt *Runtime, sid string) string {
	t.Helper()
	seats := mustSeats(t, rt, sid)
	for _, k := range seats {
		return hexOf(k)
	}
	t.Fatal("no seats")
	return ""
}

func hexOf(b []byte) string { return hex.EncodeToString(b) }

// A signature from somebody who is not at the table is not a signature, even
// when there is a real release waiting for one.
func TestAStrangersReleaseSignatureIsRefused(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := readyToRelease(t, rt, fake)
	if err := rt.Release(context.Background(), sid); err != nil {
		t.Fatalf("release: %v", err)
	}
	stranger := make([]byte, 33)
	stranger[0] = 0x02
	err := rt.adoptRelease(context.Background(), sid, schema.Release{
		Signer: hex.EncodeToString(stranger), Sig: hex.EncodeToString([]byte{1, 2, 3}),
	})
	if err == nil {
		t.Fatal("took a release signature from somebody who is not at this table")
	}
	if !strings.Contains(err.Error(), "not at this table") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	done := ourRelease(rt, sid).done
	if done {
		t.Fatal("a stranger's signature completed a release")
	}
}

// ourRelease is this seat's own release at a table.
func ourRelease(rt *Runtime, sid string) *release {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	t := rt.tables[sid]
	mine, _ := t.form.OurSeat()
	return t.releases[mine]
}

// A release nobody would have built is refused, not signed.
//
// This is the guard the whole exchange rests on. A release is co-signed by the
// other seat, so a seat that could get its opponent to sign an arbitrary
// transaction spending a bond has been handed the bond: it would name itself
// as the payee and walk away with money it never staked. The transaction is
// therefore rebuilt from what this peer knows and compared, rather than signed
// because it arrived.
func TestAReleaseThisPeerWouldNotHaveBuiltIsRefused(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := readyToRelease(t, rt, fake)
	mine := ourSeatOf(t, rt, sid)
	tbl := tableOf(t, rt, sid)

	draft, err := rt.releaseDraft(tbl, mine)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	honest, err := punish.BuildRelease(draft)
	if err != nil {
		t.Fatalf("release: %v", err)
	}

	// The same release paying a penny less, which is to say paying a penny
	// more to whoever mines it - and nothing this peer agreed to.
	forged := honest.Copy()
	forged.TxOut[0].Value--
	raw, err := forged.Bytes()
	if err != nil {
		t.Fatalf("serialise: %v", err)
	}
	theirSeat, _ := them.form.OurSeat()
	seats, _ := tbl.form.Seats()
	sig, err := escrow.SignBondSpend(forged, draft.Bond, them.creds.Session)
	if err != nil {
		t.Fatalf("their signature: %v", err)
	}
	err = rt.adoptRelease(context.Background(), sid, schema.Release{
		Seat: mine, Tx: hex.EncodeToString(raw),
		Signer: hex.EncodeToString(seats[theirSeat]), Sig: hex.EncodeToString(sig),
	})
	if err == nil {
		t.Fatal("took a signature on a release this peer would not have built")
	}
	if !strings.Contains(err.Error(), "would not have built") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	// And nothing was kept: a refused release must not leave a half-signed
	// one behind for the next message to finish.
	rt.mu.Lock()
	held := tbl.releases[mine]
	rt.mu.Unlock()
	if held != nil {
		t.Fatal("a refused release was filed anyway")
	}
}

// placeTableBond puts another seat's table bond on the chain, which is what a
// release of it needs to exist.
func placeTableBond(t *testing.T, rt *Runtime, sid string, seat uint32) {
	t.Helper()
	if _, err := rt.tableBondOf(rt.tables[sid], seat); err != nil {
		t.Fatalf("seat %d's bond: %v", seat, err)
	}
	txid := strings.Repeat("c3", 32)
	rt.mu.Lock()
	tbl := rt.tables[sid]
	if tbl.tableBondFunded == nil {
		tbl.tableBondFunded = map[uint32]staked{}
	}
	tbl.tableBondFunded[seat] = staked{outpoint: txid + ":0", atoms: int64(escrow.MinBondAtoms)}
	rt.mu.Unlock()
}

// withholding is a game that refuses to co-sign for one named seat. The seat is
// set after seating, because which seat the other peer gets is drawn from a
// block hash and is not known when the game is built.
type withholding struct {
	battleshipsRules
	mu      sync.Mutex
	against uint32
	named   bool
}

func (w *withholding) hold(seat uint32) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.against, w.named = seat, true
}

func (w *withholding) WillCoSign(_ string, seat uint32) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.named || seat != w.against
}

// A game can punish by refusing to sign, which is the only punishment some
// rulings have. The other seat's release is not co-signed and nothing is built.
func TestAGameCanWithholdASeatsRelease(t *testing.T) {
	game := &withholding{}
	fake, rt, _ := stand(t, game)
	sid, them := readyToRelease(t, rt, fake)

	// Their bond, their release, and this peer is asked to co-sign it.
	theirSeat, _ := them.form.OurSeat()
	game.hold(theirSeat)
	placeTableBond(t, rt, sid, theirSeat)
	draft, err := rt.releaseDraft(rt.tables[sid], theirSeat)
	if err != nil {
		t.Fatalf("their draft: %v", err)
	}
	tx, err := punish.BuildRelease(draft)
	if err != nil {
		t.Fatalf("build their release: %v", err)
	}
	sig, err := escrow.SignBondSpend(tx, draft.Bond, them.creds.Session)
	if err != nil {
		t.Fatalf("their signature: %v", err)
	}
	raw, err := tx.Bytes()
	if err != nil {
		t.Fatalf("serialise: %v", err)
	}
	seats, _ := them.form.Seats()
	err = rt.adoptRelease(context.Background(), sid, schema.Release{
		Seat: theirSeat, Tx: hex.EncodeToString(raw),
		Signer: hex.EncodeToString(seats[theirSeat]), Sig: hex.EncodeToString(sig),
	})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}

	rt.mu.Lock()
	held := rt.tables[sid].releases[theirSeat]
	mine, _ := rt.tables[sid].form.OurSeat()
	ourKey := hex.EncodeToString(seats[mine])
	_, signed := held.sigs[ourKey]
	sent := held.done
	rt.mu.Unlock()
	if signed {
		t.Fatal("this peer co-signed a release the game was withholding")
	}
	if sent {
		t.Fatal("a withheld release went out")
	}
}

// The same peer signs for a seat the game has not named, so withholding is a
// refusal about one seat and not a table that has stopped working.
func TestWithholdingOneSeatDoesNotStopTheOthers(t *testing.T) {
	game := &withholding{}
	fake, rt, _ := stand(t, game)
	sid, them := readyToRelease(t, rt, fake)

	// A seat that is not at this table, so nothing here is withheld.
	game.hold(9)
	theirSeat, _ := them.form.OurSeat()
	placeTableBond(t, rt, sid, theirSeat)
	draft, err := rt.releaseDraft(rt.tables[sid], theirSeat)
	if err != nil {
		t.Fatalf("their draft: %v", err)
	}
	tx, err := punish.BuildRelease(draft)
	if err != nil {
		t.Fatalf("build their release: %v", err)
	}
	sig, err := escrow.SignBondSpend(tx, draft.Bond, them.creds.Session)
	if err != nil {
		t.Fatalf("their signature: %v", err)
	}
	raw, err := tx.Bytes()
	if err != nil {
		t.Fatalf("serialise: %v", err)
	}
	seats, _ := them.form.Seats()
	if err := rt.adoptRelease(context.Background(), sid, schema.Release{
		Seat: theirSeat, Tx: hex.EncodeToString(raw),
		Signer: hex.EncodeToString(seats[theirSeat]), Sig: hex.EncodeToString(sig),
	}); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	rt.mu.Lock()
	held := rt.tables[sid].releases[theirSeat]
	mine, _ := rt.tables[sid].form.OurSeat()
	_, signed := held.sigs[hex.EncodeToString(seats[mine])]
	rt.mu.Unlock()
	if !signed {
		t.Fatal("a seat the game did not name was not co-signed")
	}
}
