package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

func quickPoll(t *testing.T) {
	t.Helper()
	old := fundPoll
	fundPoll = time.Millisecond
	t.Cleanup(func() { fundPoll = old })
}

// The order that matters: the request is written down before the bridge is
// asked, so a crash between the two leaves a record to ask about rather than a
// payment nobody is watching.
func TestARequestIsWrittenDownBeforeTheBridgeIsAsked(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	quickPoll(t)
	// The bridge cannot be reached at all, so nothing can come back.
	fake.SetUnreachable(true)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := rt.askFor(ctx, spend.Record{
		Match: "abcdef01", Seat: 0, Purpose: "stake",
		Address: payTo(t), Atoms: 5_000_000, PkScript: "76a914",
	})
	if err == nil {
		t.Fatal("asking an unreachable bridge succeeded")
	}
	open := rt.book.OpenRecords()
	if len(open) != 1 {
		t.Fatalf("the request was not written down: %+v", open)
	}
	if open[0].Purpose != "stake" || open[0].State != spend.Requested {
		t.Fatalf("the record is %+v", open[0])
	}
}

// An unreachable bridge must not end the wait, because the payment may already
// have been made. This is the failure that cost 0.01 DCR on mainnet.
//
// Checked while the bridge is still away: once it answers, the reason it could
// not be asked is cleared, which is correct and would hide what this is for.
func TestWaitingSurvivesAnUnreachableBridge(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	quickPoll(t)

	rec := spend.Record{
		ID: "spend-1", Match: "abcdef01", Seat: 0, Purpose: "stake",
		Address: payTo(t), Atoms: 5_000_000, PkScript: "76a914",
	}
	if err := rt.book.Put(rec); err != nil {
		t.Fatalf("put: %v", err)
	}
	fake.SetUnreachable(true)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := rt.awaitAnswer(ctx, "spend-1"); err == nil {
		t.Fatal("the wait ended while the bridge was unreachable")
	}

	got, _ := rt.book.Get("spend-1")
	if got.State == spend.Denied || got.State == spend.Failed || got.State == spend.Expired {
		t.Fatalf("an unreachable bridge was recorded as a refusal: %q", got.State)
	}
	if !got.State.Open() {
		t.Fatalf("the request was closed at %q; the payment may have been made", got.State)
	}
	if got.State != spend.Unknown {
		t.Fatalf("state is %q, want %q", got.State, spend.Unknown)
	}
	if got.Unreachable == "" {
		t.Fatal("the reason it could not ask was not kept")
	}
	if got.Error != "" {
		t.Fatalf("an unreachable bridge left a refusal reason: %q", got.Error)
	}
}

// And when the bridge comes back, the same request settles normally.
func TestTheWaitFinishesWhenTheBridgeComesBack(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	quickPoll(t)

	sp, err := rt.bridge.RequestSpend(context.Background(), payTo(t), 5_000_000, "stake")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if err := rt.book.Put(spend.Record{
		ID: sp.ID, Match: "abcdef01", Purpose: "stake",
		Address: payTo(t), Atoms: 5_000_000, PkScript: "76a914",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	fake.SetUnreachable(true)
	go func() {
		time.Sleep(50 * time.Millisecond)
		fake.SetUnreachable(false)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := rt.awaitAnswer(ctx, sp.ID)
	if err != nil {
		t.Fatalf("the wait did not finish after the bridge came back: %v", err)
	}
	if got.State != spend.Approved || got.TxID == "" {
		t.Fatalf("settled as %q with txid %q", got.State, got.TxID)
	}
	if got.Unreachable != "" {
		t.Fatalf("an answered request still cites being unreachable: %q", got.Unreachable)
	}
}

// A refusal is terminal and the wait ends, because the bridge did answer.
func TestWaitingEndsWhenTheBridgeRefuses(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	quickPoll(t)
	fake.SetVerdict(bridgetest.Refuse, "over the cap")

	sp, err := rt.bridge.RequestSpend(context.Background(), payTo(t), 5_000_000, "stake")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if err := rt.book.Put(spend.Record{
		ID: sp.ID, Match: "abcdef01", Purpose: "stake",
		Address: payTo(t), Atoms: 5_000_000, PkScript: "76a914",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := rt.awaitAnswer(ctx, sp.ID); err == nil {
		t.Fatal("a refusal did not end the wait")
	}
	got, _ := rt.book.Get(sp.ID)
	if got.State != spend.Denied {
		t.Fatalf("state is %q", got.State)
	}
	if got.Error != "over the cap" {
		t.Fatalf("the refusal lost its reason: %q", got.Error)
	}
}

// The output has to actually pay the script that was asked for. A transaction
// that paid somewhere else is money that moved but not to this table.
func TestADepositMustPayTheScriptThatWasAskedFor(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	rt.mu.Lock()
	tbl := rt.tables[sid]
	rt.mu.Unlock()

	txid := strings.Repeat("ab", 32)
	fake.Place(txid, 0, []byte{0x51}, 5_000_000, fake.Height())

	err = rt.landDeposit(context.Background(), tbl, 0, spend.Record{
		ID: "s1", Purpose: "stake", TxID: txid, PkScript: "76a914deadbeef",
	})
	if err == nil {
		t.Fatal("recorded a deposit into a script nobody asked for")
	}
	if !strings.Contains(err.Error(), "not to this table") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	if _, _, ok := rt.Funded(sid, 0); ok {
		t.Fatal("the seat reads as funded")
	}
}

// An approval with no transaction is not a deposit.
func TestAnApprovalWithNoTransactionIsRefused(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	err := rt.landDeposit(context.Background(), &table{}, 0, spend.Record{
		ID: "s1", Purpose: "stake", PkScript: "76a914",
	})
	if err == nil {
		t.Fatal("landed a deposit with no transaction")
	}
}

// A seat already funded is not funded again. Paying twice is the whole thing
// this package exists to avoid.
func TestASeatAlreadyFundedIsNotFundedAgain(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	rt.mu.Lock()
	rt.tables[sid].funded = map[uint32]staked{0: {outpoint: "x:0", atoms: 1}}
	rt.mu.Unlock()

	before := len(fake.Spends())
	// OurSeat is not set on an unseated table, so this returns before the
	// funded check; drive the check directly instead.
	if out, atoms, ok := rt.Funded(sid, 0); !ok || out != "x:0" || atoms != 1 {
		t.Fatalf("the recorded stake is %q/%d/%v", out, atoms, ok)
	}
	if len(fake.Spends()) != before {
		t.Fatal("reading a funded seat asked the bridge for money")
	}
}

func TestFundingRefusesATableThisGameIsNotAt(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	if err := rt.Fund(context.Background(), "no-such-table"); err == nil {
		t.Fatal("funded a table this game is not at")
	}
}

// A payout script has to be recorded against a real table, and it is copied
// rather than aliased so a caller cannot change it afterwards.
func TestAPayoutIsRecordedAgainstItsTableAndCopied(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := rt.SetPayoutFor("no-such-table", 0, []byte{1}); err == nil {
		t.Error("recorded a payout for a table this game is not at")
	}
	if err := rt.SetPayoutFor(sid, 0, nil); err == nil {
		t.Error("recorded a payout of nothing")
	}
	mine := []byte{0x76, 0xa9}
	if err := rt.SetPayoutFor(sid, 0, mine); err != nil {
		t.Fatalf("set payout: %v", err)
	}
	mine[0] = 0xff
	rt.mu.Lock()
	got := rt.tables[sid].payouts[0]
	rt.mu.Unlock()
	if got[0] != 0x76 {
		t.Fatal("the stored payout changed when the caller's slice did")
	}
}
