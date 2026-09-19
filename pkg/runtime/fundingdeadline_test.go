package runtime

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/txscript/v4/stdaddr"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

// fundSeats is fundBoth for a chosen subset, which is the whole point here:
// the table this file is about is one where somebody did not pay.
func fundSeats(t *testing.T, fake *bridgetest.Bridge, rt *Runtime, sid string, seats ...uint32) {
	t.Helper()
	want := map[uint32]bool{}
	for _, seat := range seats {
		want[seat] = true
	}
	rt.mu.Lock()
	tbl := rt.tables[sid]
	rt.mu.Unlock()

	deposits, err := tbl.formation().Deposits(chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("deposits: %v", err)
	}
	pay, err := stdaddr.DecodeAddress(payTo(t), chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("payout: %v", err)
	}
	_, payScript := pay.PaymentScript()

	rt.mu.Lock()
	tbl.funded = map[uint32]staked{}
	tbl.payouts = map[uint32][]byte{}
	rt.mu.Unlock()

	for _, d := range deposits {
		if !want[d.Seat] {
			continue
		}
		txid := stakeTxid(d.Seat)
		script, err := hex.DecodeString(d.PkScriptHex)
		if err != nil {
			t.Fatalf("seat %d pkScript: %v", d.Seat, err)
		}
		fake.Place(txid, 0, script, stakeAtoms, fake.Height())
		rt.mu.Lock()
		tbl.funded[d.Seat] = staked{outpoint: txid + ":0", atoms: stakeAtoms}
		tbl.payouts[d.Seat] = payScript
		rt.mu.Unlock()
	}
}

func lapseState(t *testing.T, rt *Runtime, sid string) (bool, string) {
	t.Helper()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	tbl := rt.tables[sid]
	if tbl == nil {
		t.Fatal("no table")
	}
	return tbl.recoveryOnly, tbl.recoveryReason
}

// The seat that did pay is told, at the deadline, that nobody else will.
// Before this the table sat seated and silent, and the only way to find out
// was to notice the game had been waiting for an hour.
func TestASeatThatNeverFundsLapsesTheTable(t *testing.T) {
	dir, store := t.TempDir(), NewMemTableStore()
	fake, rt := standAt(t, &trivialGame{}, dir, store)
	sid, _ := seatTwo(t, fake, rt)
	fundSeats(t, fake, rt, sid, 0)

	terms := membership.Terms{}
	rt.mu.Lock()
	terms = rt.tables[sid].formation().Terms()
	rt.mu.Unlock()
	deadline := int64(membership.FundingDeadline(terms))

	fake.SetHeight(deadline)
	rt.Tick(context.Background(), deadline)
	if only, _ := lapseState(t, rt, sid); only {
		t.Fatal("the table was given up on before its funding deadline")
	}

	fake.SetHeight(deadline + 1)
	rt.Tick(context.Background(), deadline+1)
	only, reason := lapseState(t, rt, sid)
	if !only {
		t.Fatal("an unfunded seat held the table past the funding deadline")
	}
	if !strings.Contains(reason, "1 of 2") {
		t.Fatalf("the reason does not name the shortfall: %q", reason)
	}

	snap, err := rt.Snapshot(sid)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Phase != "recovery" {
		t.Fatalf("a lapsed table reads as %q, so the game still shows it as playable", snap.Phase)
	}

	// The membership is what the refund script is derived from. Giving up on
	// the table must not forget where the funded seat's money is.
	rt.mu.Lock()
	tbl := rt.tables[sid]
	rt.mu.Unlock()
	if got := tbl.formation().State(); got != membership.Aborted {
		t.Fatalf("the formation is %s, not abandoned", got)
	}
	if _, err := tbl.formation().Deposits(chaincfg.TestNet3Params()); err != nil {
		t.Fatalf("the funded seat can no longer derive its refund: %v", err)
	}
}

// A table everybody paid for is not on a clock. It waits for the game.
func TestAFullyFundedTableOutlivesTheFundingDeadline(t *testing.T) {
	dir, store := t.TempDir(), NewMemTableStore()
	fake, rt := standAt(t, &trivialGame{}, dir, store)
	sid, them := seatTwo(t, fake, rt)
	fundBoth(t, fake, rt, sid, them)

	rt.mu.Lock()
	terms := rt.tables[sid].formation().Terms()
	rt.mu.Unlock()

	for h := int64(membership.FundingDeadline(terms)); h < int64(membership.FundingDeadline(terms))+64; h++ {
		fake.SetHeight(h)
		rt.Tick(context.Background(), h)
	}
	if only, reason := lapseState(t, rt, sid); only {
		t.Fatalf("a fully funded table was given up on: %s", reason)
	}
}

// A lapse is durable. A restart that forgot it would put the game back to
// waiting for a stake that is never coming.
func TestALapsedTableStaysLapsedAcrossARestart(t *testing.T) {
	dir, store := t.TempDir(), NewMemTableStore()
	fake, rt := standAt(t, &trivialGame{}, dir, store)
	sid, _ := seatTwo(t, fake, rt)
	fundSeats(t, fake, rt, sid, 0)

	rt.mu.Lock()
	terms := rt.tables[sid].formation().Terms()
	rt.mu.Unlock()
	past := int64(membership.FundingDeadline(terms)) + 1
	fake.SetHeight(past)
	rt.Tick(context.Background(), past)
	if only, _ := lapseState(t, rt, sid); !only {
		t.Fatal("the table did not lapse")
	}

	_, again := standAt(t, &trivialGame{}, dir, store)
	if err := again.Resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	only, reason := lapseState(t, again, sid)
	if !only {
		t.Fatal("a restart brought a lapsed table back as if it were live")
	}
	if !strings.Contains(reason, "1 of 2") {
		t.Fatalf("the restart lost the reason: %q", reason)
	}
	again.mu.Lock()
	tbl := again.tables[sid]
	again.mu.Unlock()
	if _, err := tbl.formation().Deposits(chaincfg.TestNet3Params()); err != nil {
		t.Fatalf("the restart lost the membership the refund is derived from: %v", err)
	}
}
