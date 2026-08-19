package runtime

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/decred/dcrd/wire"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
)

const stakeAtoms = 5_000_000

// An outcome that does not add up is one the other seats refuse, and a table
// whose seats cannot agree falls back to its refund timelocks.
func TestAnOutcomeThatDoesNotAddUpIsRefused(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	tbl := fakeSeated(t, rt, 2)

	for _, tc := range []struct {
		name   string
		shares map[uint32]int64
	}{
		{"paying out more than the table holds", map[uint32]int64{0: stakeAtoms * 2, 1: stakeAtoms}},
		{"paying out less than the table holds", map[uint32]int64{0: 1, 1: 1}},
		{"a negative share", map[uint32]int64{0: -1, 1: stakeAtoms*2 + 1}},
		{"a seat that is not at this table", map[uint32]int64{0: stakeAtoms, 9: stakeAtoms}},
	} {
		_, err := rt.sharesFor(tbl, []uint32{0, 1}, Outcome{Shares: tc.shares})
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

// Winner-take-all, a split and a draw all have to be expressible, which is why
// the outcome is shares rather than a winner.
func TestEveryDivisionOfThePotIsExpressible(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	tbl := fakeSeated(t, rt, 2)
	pot := int64(stakeAtoms * 2)

	for _, tc := range []struct {
		name   string
		shares map[uint32]int64
	}{
		{"winner takes all", map[uint32]int64{0: pot, 1: 0}},
		{"the other seat wins", map[uint32]int64{0: 0, 1: pot}},
		{"a draw", map[uint32]int64{0: pot / 2, 1: pot / 2}},
		{"an uneven split", map[uint32]int64{0: pot / 4 * 3, 1: pot / 4}},
	} {
		got, err := rt.sharesFor(tbl, []uint32{0, 1}, Outcome{Shares: tc.shares})
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		var total int64
		for _, v := range got {
			total += v
		}
		if total != pot {
			t.Errorf("%s: divides %d of %d", tc.name, total, pot)
		}
	}
}

// A void table is not an even split and not a refusal: every seat takes its own
// stake back, which is the one division nobody can dispute.
func TestAVoidTableReturnsEverySeatsOwnStake(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	tbl := fakeSeated(t, rt, 2)
	tbl.funded[1] = staked{outpoint: stakeOutpoint(1), atoms: stakeAtoms * 3}

	got, err := rt.sharesFor(tbl, []uint32{0, 1}, Outcome{Void: true})
	if err != nil {
		t.Fatalf("void: %v", err)
	}
	if got[0] != stakeAtoms || got[1] != stakeAtoms*3 {
		t.Fatalf("a void table did not return each stake: %v", got)
	}
}

func TestSettlingRefusesWhatCannotBeSettled(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		match string
		out   Outcome
	}{
		{"no table named", "", Outcome{Void: true}},
		{"an outcome that decides nothing", "abcdef01", Outcome{}},
		{"a table this game is not at", "no-such-table", Outcome{Void: true}},
	} {
		if err := rt.Settle(ctx, tc.match, tc.out); err == nil {
			t.Errorf("%s: settled", tc.name)
		}
	}
}

// A seat whose stake is not on the chain cannot be settled, and the refusal
// says which stage is missing rather than only that it failed.
func TestASeatWithNoStakeOnChainCannotBeSettled(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	err = rt.Settle(context.Background(), sid, Outcome{Void: true})
	if err == nil {
		t.Fatal("settled a table nobody had funded")
	}
	// Not ErrNotYet: settlement is built, this table is simply not ready.
	if errors.Is(err, ErrNotYet) {
		t.Fatalf("an unfunded table reported settlement as unbuilt: %v", err)
	}
	if !strings.Contains(err.Error(), "seating") && !strings.Contains(err.Error(), "not on the chain") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// fakeSeated builds a table whose funding and payouts are filled in directly,
// so the arithmetic can be tested without the funding stage.
func fakeSeated(t *testing.T, rt *Runtime, seats uint32) *table {
	t.Helper()
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	tbl := rt.tables[sid]
	tbl.funded = map[uint32]staked{}
	tbl.payouts = map[uint32][]byte{}
	for seat := range seats {
		tbl.funded[seat] = staked{outpoint: stakeOutpoint(seat), atoms: stakeAtoms}
	}
	return tbl
}

func stakeOutpoint(seat uint32) string {
	return strings.Repeat("cd", 32) + ":" + strconv.Itoa(int(seat))
}

// The order inputs are built in decides the transaction's bytes, so every peer
// has to reach the same one. A Go map iterated directly would not.
func TestASettlementIsBuiltInSeatOrder(t *testing.T) {
	seats := map[uint32][]byte{3: {3}, 0: {0}, 5: {5}, 1: {1}}
	for range 20 {
		got := seatOrder(seats)
		want := []uint32{0, 1, 3, 5}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v - two peers would build different bytes", got, want)
			}
		}
	}
}

// The load-bearing check on the receiving side: a seat that could get the
// others to sign a payout they had not computed themselves could pay itself the
// table. So a proposal is rebuilt locally and compared before anything is
// signed.
func TestAPayoutThisPeerWouldNotHaveBuiltIsNotSigned(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	err = rt.adoptSettlement(context.Background(), sid, schema.Settle{
		Tx: "00", Signer: "aa", Sigs: []string{"bb"},
	})
	if err == nil {
		t.Fatal("adopted a payout from a stranger at an unseated table")
	}
}

func TestAdoptingAPayoutRefusesTheObviouslyWrong(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	for _, tc := range []struct {
		name  string
		match string
		body  schema.Settle
	}{
		{"a table this game is not at", "no-such-table", schema.Settle{}},
		{"a signer that is not hex", sid, schema.Settle{Signer: "zz"}},
		{"a transaction that is not hex", sid, schema.Settle{Signer: "aa", Tx: "zz"}},
	} {
		if err := rt.adoptSettlement(context.Background(), tc.match, tc.body); err == nil {
			t.Errorf("%s: adopted", tc.name)
		}
	}
}

// Being short of a signature is not an error - the missing seat has not spoken
// yet - and nothing is broadcast until everybody has.
func TestAPayoutShortOfASignatureIsNotSent(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	tbl := fakeSeated(t, rt, 2)
	tbl.settle = &settlement{
		draft: escrow.SettleDraft{Inputs: []escrow.SettleInput{{Redeem: []byte{0x51}}}},
		sigs:  map[string][][]byte{},
	}
	before := len(fake.Spends())
	if err := rt.completeSettlement(context.Background(), tbl); err != nil {
		// Members() on a nonsense redeem may refuse, which is also "not sent".
		if !strings.Contains(err.Error(), "member") && !strings.Contains(err.Error(), "script") {
			t.Fatalf("unexpected: %v", err)
		}
	}
	if tbl.settle.done {
		t.Fatal("a payout short of a signature was marked sent")
	}
	if len(fake.Spends()) != before {
		t.Fatal("a payout short of a signature asked the bridge for money")
	}
}

// A payout already sent is not sent twice.
func TestAPayoutAlreadySentIsNotSentAgain(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	tbl := fakeSeated(t, rt, 2)
	tbl.settle = &settlement{done: true}
	if err := rt.completeSettlement(context.Background(), tbl); err != nil {
		t.Fatalf("completing a finished settlement: %v", err)
	}
}

// theirSettle is the other seat signing the payout our side proposed.
func theirSettle(t *testing.T, rt *Runtime, sid string, them *peer) schema.Settle {
	t.Helper()
	rt.mu.Lock()
	s := rt.tables[sid].settle
	rt.mu.Unlock()
	if s == nil {
		t.Fatal("our side proposed no payout")
	}
	sigs, err := escrow.SignSettlement(s.tx, s.draft, them.creds.Session)
	if err != nil {
		t.Fatalf("their signature: %v", err)
	}
	raw, err := s.tx.Bytes()
	if err != nil {
		t.Fatalf("serialise: %v", err)
	}
	mine, _ := them.form.OurSeat()
	seats, _ := them.form.Seats()
	// The other seat is whichever one is not ours.
	var theirKey []byte
	for seat, k := range seats {
		if seat != ourSeatOf(t, rt, sid) {
			theirKey = k
		}
	}
	_ = mine
	hexSigs := make([]string, 0, len(sigs))
	for _, sig := range sigs {
		hexSigs = append(hexSigs, hex.EncodeToString(sig))
	}
	return schema.Settle{
		Tx: hex.EncodeToString(raw), Signer: hex.EncodeToString(theirKey), Sigs: hexSigs,
	}
}

func ourSeatOf(t *testing.T, rt *Runtime, sid string) uint32 {
	t.Helper()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	seat, ok := rt.tables[sid].form.OurSeat()
	if !ok {
		t.Fatal("we have no seat")
	}
	return seat
}

// The whole path: both seats sign a payout they each computed, and it goes out.
func TestATableBothSeatsAgreeOnIsPaidOut(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	fundBoth(t, fake, rt, sid, them)

	pot := int64(stakeAtoms * 2)
	winner := ourSeatOf(t, rt, sid)
	shares := map[uint32]int64{winner: pot}
	for seat := range mustSeats(t, rt, sid) {
		if seat != winner {
			shares[seat] = 0
		}
	}
	if err := rt.Settle(context.Background(), sid, Outcome{Shares: shares}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	rt.mu.Lock()
	sent := rt.tables[sid].settle.done
	rt.mu.Unlock()
	if sent {
		t.Fatal("a payout went out with only one signature")
	}

	if err := rt.adoptSettlement(context.Background(), sid, theirSettle(t, rt, sid, them)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	rt.mu.Lock()
	sent = rt.tables[sid].settle.done
	rt.mu.Unlock()
	if !sent {
		t.Fatal("a fully signed payout was not sent")
	}
	if len(fake.Spends()) != 0 {
		t.Fatal("a settlement asked the bridge to spend rather than broadcasting")
	}
}

// The check that stops a seat paying itself the table: a proposal is rebuilt
// locally and compared before anything is signed.
func TestAPayoutWithTamperedAmountsIsNotSigned(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	fundBoth(t, fake, rt, sid, them)

	winner := ourSeatOf(t, rt, sid)
	shares := map[uint32]int64{winner: stakeAtoms * 2}
	for seat := range mustSeats(t, rt, sid) {
		if seat != winner {
			shares[seat] = 0
		}
	}
	if err := rt.Settle(context.Background(), sid, Outcome{Shares: shares}); err != nil {
		t.Fatalf("settle: %v", err)
	}

	body := theirSettle(t, rt, sid, them)
	// Move a coin in the transaction they are asking us to agree with.
	raw, err := hex.DecodeString(body.Tx)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	tx := wire.NewMsgTx()
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		t.Fatalf("deserialise: %v", err)
	}
	tx.TxOut[0].Value -= 1000
	tampered, err := tx.Bytes()
	if err != nil {
		t.Fatalf("serialise: %v", err)
	}
	body.Tx = hex.EncodeToString(tampered)

	err = rt.adoptSettlement(context.Background(), sid, body)
	if err == nil {
		t.Fatal("signed a payout this peer would not have built")
	}
	if !strings.Contains(err.Error(), "would not have built") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	rt.mu.Lock()
	sent := rt.tables[sid].settle.done
	rt.mu.Unlock()
	if sent {
		t.Fatal("a tampered payout was sent")
	}
}

// A signature from somebody who is not at the table is not a signature.
func TestASignerWhoIsNotAtTheTableIsRefused(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	fundBoth(t, fake, rt, sid, them)
	settleOurs(t, rt, sid)

	body := theirSettle(t, rt, sid, them)
	stranger := make([]byte, 33)
	stranger[0] = 0x02
	body.Signer = hex.EncodeToString(stranger)
	if err := rt.adoptSettlement(context.Background(), sid, body); err == nil {
		t.Fatal("took a signature from somebody who is not at this table")
	}
}

// A payout over two inputs signed once is not a signed payout.
func TestTheWrongNumberOfSignaturesIsRefused(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	fundBoth(t, fake, rt, sid, them)
	settleOurs(t, rt, sid)

	body := theirSettle(t, rt, sid, them)
	body.Sigs = body.Sigs[:1]
	err := rt.adoptSettlement(context.Background(), sid, body)
	if err == nil {
		t.Fatal("took a payout with too few signatures")
	}
	if !strings.Contains(err.Error(), "signatures") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

func settleOurs(t *testing.T, rt *Runtime, sid string) {
	t.Helper()
	winner := ourSeatOf(t, rt, sid)
	shares := map[uint32]int64{winner: stakeAtoms * 2}
	for seat := range mustSeats(t, rt, sid) {
		if seat != winner {
			shares[seat] = 0
		}
	}
	if err := rt.Settle(context.Background(), sid, Outcome{Shares: shares}); err != nil {
		t.Fatalf("settle: %v", err)
	}
}

func mustSeats(t *testing.T, rt *Runtime, sid string) map[uint32][]byte {
	t.Helper()
	seats, ok := rt.Seats(sid)
	if !ok {
		t.Fatal("the table has no seats")
	}
	return seats
}
