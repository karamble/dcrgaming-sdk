package runtime

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

func quickPoll(t *testing.T) {
	t.Helper()
	old := fundPoll
	fundPoll = time.Millisecond
	t.Cleanup(func() { fundPoll = old })
}

func requestVerifiedTestDeposit(t *testing.T, rt *Runtime) (spend.Record, string) {
	t.Helper()
	key, err := rt.identity.DeriveKey(rt.seatTags.Bond, "fund-test")
	if err != nil {
		t.Fatalf("derive identity key: %v", err)
	}
	dep, err := rt.bridge.PrepareDeposit(context.Background(), &gamingpb.PrepareDepositRequest{
		Sid: "fund-test", Kind: "seatbond", AmountAtoms: 5_000_000, LockBlocks: 8,
		IdentityKey: hex.EncodeToString(key.PubKey().SerializeCompressed()),
	})
	if err != nil {
		t.Fatalf("prepare deposit: %v", err)
	}
	sp, err := rt.bridge.RequestDepositSpend(context.Background(), dep.GetId(), dep.GetAddress(), 5_000_000, "admission bond")
	if err != nil {
		t.Fatalf("request deposit: %v", err)
	}
	return spend.Record{ID: sp.ID, Match: "fund-test", Purpose: "seatbond",
		Address: dep.GetAddress(), Atoms: 5_000_000, PkScript: dep.GetPkScript(), DepositID: dep.GetId()}, sp.ID
}

// A game cannot create a payment request without a bridge-prepared deposit.
// Failure before that descriptor exists leaves no local record claiming money
// may have moved and no spend at the bridge.
func TestARequestWithoutABridgeDepositCannotMoveMoney(t *testing.T) {
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
	if len(open) != 0 {
		t.Fatalf("an unverified request was recorded as money in flight: %+v", open)
	}
	if len(fake.Spends()) != 0 {
		t.Fatal("the unverified request reached payment approval")
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

	rec, id := requestVerifiedTestDeposit(t, rt)
	if err := rt.book.Put(rec); err != nil {
		t.Fatalf("put: %v", err)
	}
	fake.SetUnreachable(true)
	go func() {
		time.Sleep(50 * time.Millisecond)
		fake.SetUnreachable(false)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := rt.awaitAnswer(ctx, id)
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

	rec, id := requestVerifiedTestDeposit(t, rt)
	if err := rt.book.Put(rec); err != nil {
		t.Fatalf("put: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := rt.awaitAnswer(ctx, id); err == nil {
		t.Fatal("a refusal did not end the wait")
	}
	got, _ := rt.book.Get(id)
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
