package runtime

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/punish"
	"github.com/karamble/dcrgaming-sdk/pkg/ruling"
)

// A clean ruling releases the table bond, and it needs the bond on the chain
// and somewhere to pay it.
func TestACleanRulingNeedsTheBondAndAPayout(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := seatTwo(t, fake, rt)

	err := rt.Forfeit(context.Background(), ruling.Ruling{Match: sid, Kind: ruling.Clean})
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

	if err := rt.Forfeit(context.Background(), ruling.Ruling{Match: sid, Kind: ruling.Clean}); err != nil {
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

	if err := rt.Forfeit(context.Background(), ruling.Ruling{Match: sid, Kind: ruling.Clean}); err != nil {
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
	if err := rt.Forfeit(context.Background(), ruling.Ruling{Match: sid, Kind: ruling.Clean}); err != nil {
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
