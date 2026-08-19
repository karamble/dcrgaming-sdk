package runtime

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/ruling"
)

// bondedTwo seats two, sets payouts, and puts BOTH table bonds on the chain -
// the state a pre-signed accusation chain needs.
func bondedTwo(t *testing.T, rt *Runtime, fake *bridgetest.Bridge) (string, *peer) {
	t.Helper()
	sid, them := readyToRelease(t, rt, fake)

	// The other seat's bond, which our chain will be built against.
	theirSeat, _ := them.form.OurSeat()
	bond, err := rt.tableBondOf(tableOf(t, rt, sid), theirSeat)
	if err != nil {
		t.Fatalf("their bond: %v", err)
	}
	script, err := hex.DecodeString(bond.PkScriptHex)
	if err != nil {
		t.Fatalf("their pkScript: %v", err)
	}
	txid := strings.Repeat("e1", 32)
	fake.Place(txid, 0, script, int64(escrow.MinBondAtoms), fake.Height())
	tbl := tableOf(t, rt, sid)
	rt.mu.Lock()
	tbl.tableBondFunded[theirSeat] = staked{
		outpoint: txid + ":0", atoms: int64(escrow.MinBondAtoms),
	}
	rt.mu.Unlock()
	return sid, them
}

func tableOf(t *testing.T, rt *Runtime, sid string) *table {
	t.Helper()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	tbl, ok := rt.tables[sid]
	if !ok {
		t.Fatalf("no table %q", sid)
	}
	return tbl
}

// The chain is pre-signed at bonding, before anybody has misbehaved, because a
// seat that has gone silent will not be signing anything.
func TestTheChainIsPresignedAtBonding(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := bondedTwo(t, rt, fake)

	if err := rt.PresignLadder(context.Background(), sid); err != nil {
		t.Fatalf("presign: %v", err)
	}
	tbl := tableOf(t, rt, sid)
	rt.mu.Lock()
	l := tbl.ladder
	rt.mu.Unlock()
	if l == nil || len(l.rungs) == 0 {
		t.Fatal("no chain was built")
	}
	ready, run, ok := rt.Ladder(sid)
	if !ok {
		t.Fatal("the table reports no chain")
	}
	if ready != 0 {
		t.Fatalf("%d rungs are ready with only one signature", ready)
	}
	if run != 0 {
		t.Fatalf("%d rungs were run before anybody misbehaved", run)
	}
}

// A rung this peer did not build is not signed. A seat that could get the other
// to sign one it had not computed would only need it once.
func TestARungThisPeerDidNotBuildIsRefused(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := bondedTwo(t, rt, fake)
	if err := rt.PresignLadder(context.Background(), sid); err != nil {
		t.Fatalf("presign: %v", err)
	}
	seats, _ := them.form.Seats()
	theirSeat, _ := them.form.OurSeat()

	err := rt.adoptAccusation(context.Background(), sid, schema.Accusation{
		Seat: 0, Tx: "00", Signer: hex.EncodeToString(seats[theirSeat]), Sig: "aabb",
	})
	if err == nil {
		t.Fatal("signed a rung this peer did not build")
	}
	if !strings.Contains(err.Error(), "not a rung of the chain") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

func TestAnAccusationFromAStrangerIsRefused(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := bondedTwo(t, rt, fake)
	if err := rt.PresignLadder(context.Background(), sid); err != nil {
		t.Fatalf("presign: %v", err)
	}
	stranger := make([]byte, 33)
	stranger[0] = 0x02
	// A real rung, so only the signer check can refuse it.
	tbl := tableOf(t, rt, sid)
	rt.mu.Lock()
	raw, err := tbl.ladder.rungs[0].Bytes()
	rt.mu.Unlock()
	if err != nil {
		t.Fatalf("serialise: %v", err)
	}
	err = rt.adoptAccusation(context.Background(), sid, schema.Accusation{
		Signer: hex.EncodeToString(stranger), Sig: "aabb", Tx: hex.EncodeToString(raw),
	})
	if err == nil {
		t.Fatal("took an accusation signature from somebody who is not at this table")
	}
	if !strings.Contains(err.Error(), "not at this table") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// Nothing runs before the chain is co-signed: a rung with one signature does
// not satisfy the bond.
func TestNoRungRunsBeforeItIsCoSigned(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := bondedTwo(t, rt, fake)
	if err := rt.PresignLadder(context.Background(), sid); err != nil {
		t.Fatalf("presign: %v", err)
	}
	mine := ourSeatOf(t, rt, sid)
	err := rt.Forfeit(context.Background(), ruling.Ruling{
		Match: sid, Against: 1 - mine, Kind: ruling.Silence,
		Silent: &ruling.Silent{Duty: "place", Seq: 1, By: 900},
	})
	if err == nil {
		t.Fatal("ran a rung nobody had co-signed")
	}
	if !strings.Contains(err.Error(), "not co-signed") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// A silence ruling against a seat the chain was not built against is refused,
// so a chain cannot be pointed at the wrong bond.
func TestAChainCannotBePointedAtAnotherSeat(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := bondedTwo(t, rt, fake)
	if err := rt.PresignLadder(context.Background(), sid); err != nil {
		t.Fatalf("presign: %v", err)
	}
	mine := ourSeatOf(t, rt, sid)
	err := rt.Forfeit(context.Background(), ruling.Ruling{
		Match: sid, Against: mine, Kind: ruling.Silence,
		Silent: &ruling.Silent{Duty: "place", Seq: 1, By: 900},
	})
	if err == nil {
		t.Fatal("ran a chain against the wrong seat")
	}
	if !strings.Contains(err.Error(), "is against seat") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// A table with no chain has nothing to run.
func TestRunningAChainThatWasNeverBuiltIsRefused(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := seatTwo(t, fake, rt)
	err := rt.Forfeit(context.Background(), ruling.Ruling{
		Match: sid, Against: 1, Kind: ruling.Silence,
		Silent: &ruling.Silent{Duty: "place", Seq: 1, By: 900},
	})
	if err == nil {
		t.Fatal("ran a chain that was never built")
	}
}

// The chain cannot be pre-signed against a bond that is not on the chain.
func TestAChainNeedsTheBondOnTheChain(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := seatTwo(t, fake, rt)
	if err := rt.PresignLadder(context.Background(), sid); err == nil {
		t.Fatal("built a chain against a bond nobody had funded")
	}
}

// An answer and a take both refuse an output that is not a rung of this chain.
// Getting this wrong means signing a spend of somebody else's coin.
func TestAnswerAndTakeRefuseAnOutputThatIsNotARung(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := bondedTwo(t, rt, fake)
	if err := rt.PresignLadder(context.Background(), sid); err != nil {
		t.Fatalf("presign: %v", err)
	}
	if err := rt.setPayout(context.Background(), payTo(t)); err != nil {
		t.Fatalf("payout: %v", err)
	}
	// Somebody else's output.
	other := strings.Repeat("f2", 32)
	fake.Place(other, 0, []byte{0x51}, 100_000, fake.Height())
	op := other + ":0"

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"answer", func() error { return rt.AnswerAccusation(context.Background(), sid, op) }},
		{"take", func() error { return rt.TakeExpiredClaim(context.Background(), sid, op) }},
	} {
		err := tc.run()
		if err == nil {
			t.Errorf("%s: signed a spend of an output that is not a rung", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "not a rung of this chain") {
			t.Errorf("%s: refused for the wrong reason: %v", tc.name, err)
		}
	}
}

// Neither spends an output that is not there.
func TestAnswerAndTakeRefuseAnOutputThatIsNotThere(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := bondedTwo(t, rt, fake)
	if err := rt.PresignLadder(context.Background(), sid); err != nil {
		t.Fatalf("presign: %v", err)
	}
	if err := rt.setPayout(context.Background(), payTo(t)); err != nil {
		t.Fatalf("payout: %v", err)
	}
	op := strings.Repeat("f3", 32) + ":0"
	// The script check below would refuse an empty output too; pin which
	// refusal this is, so the not-found guard is the one being tested.
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"answer", func() error { return rt.AnswerAccusation(context.Background(), sid, op) }},
		{"take", func() error { return rt.TakeExpiredClaim(context.Background(), sid, op) }},
	} {
		err := tc.run()
		if err == nil {
			t.Errorf("%s: spent an output that is not there", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "holds no coin") {
			t.Errorf("%s: refused for the wrong reason: %v", tc.name, err)
		}
	}
}

// This process does not spend the same output twice.
func TestAnswerAndTakeWillNotSpendTheSameOutputTwice(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := bondedTwo(t, rt, fake)
	op := strings.Repeat("f4", 32) + ":0"
	rt.noteSweeping(op)

	if err := rt.AnswerAccusation(context.Background(), sid, op); err == nil ||
		!strings.Contains(err.Error(), "already on its way") {
		t.Errorf("answer: %v", err)
	}
	if err := rt.TakeExpiredClaim(context.Background(), sid, op); err == nil ||
		!strings.Contains(err.Error(), "already on its way") {
		t.Errorf("take: %v", err)
	}
}

// A seat cannot take a claim against itself.
func TestASeatCannotTakeAClaimAgainstItself(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := bondedTwo(t, rt, fake)
	if err := rt.PresignLadder(context.Background(), sid); err != nil {
		t.Fatalf("presign: %v", err)
	}
	tbl := tableOf(t, rt, sid)
	rt.mu.Lock()
	tbl.ladder.against = ourSeatOfLocked(tbl)
	rt.mu.Unlock()
	err := rt.TakeExpiredClaim(context.Background(), sid, strings.Repeat("f5", 32)+":0")
	if err == nil || !strings.Contains(err.Error(), "against itself") {
		t.Fatalf("took a claim against itself: %v", err)
	}
}

func ourSeatOfLocked(t *table) uint32 {
	seat, _ := t.form.OurSeat()
	return seat
}
