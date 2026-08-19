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
	rel := rt.tables[sid].release
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
	body := schema.Release{
		Signer: hex.EncodeToString(seats[theirSeat]), Sig: hex.EncodeToString(sig),
	}
	if err := rt.adoptRelease(context.Background(), sid, body); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	rt.mu.Lock()
	sent = rt.tables[sid].release.done
	rt.mu.Unlock()
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
	rt.mu.Lock()
	done := rt.tables[sid].release.done
	rt.mu.Unlock()
	if done {
		t.Fatal("a release short of a signature was sent")
	}

	// And the owner can still take it alone at the lock.
	if err := rt.BackstopRelease(context.Background(), sid); err != nil {
		t.Fatalf("backstop: %v", err)
	}
	rt.mu.Lock()
	done = rt.tables[sid].release.done
	rt.mu.Unlock()
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
	tbl.release = &release{done: true}
	rt.mu.Unlock()
	if err := rt.completeRelease(context.Background(), tbl); err != nil {
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
	rt.mu.Lock()
	done := rt.tables[sid].release.done
	rt.mu.Unlock()
	if done {
		t.Fatal("a stranger's signature completed a release")
	}
}
