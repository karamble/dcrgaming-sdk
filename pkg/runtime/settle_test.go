package runtime

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

const stakeAtoms = 5_000_000

func TestAnOutcomeThatDoesNotAddUpIsRefused(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	tbl := fakeFundedTable(t, rt, 2)
	for _, tc := range []struct {
		name   string
		shares map[uint32]int64
	}{
		{"paying out more than the table holds", map[uint32]int64{0: stakeAtoms * 2, 1: stakeAtoms}},
		{"paying out less than the table holds", map[uint32]int64{0: 1, 1: 1}},
		{"a negative share", map[uint32]int64{0: -1, 1: stakeAtoms*2 + 1}},
		{"a seat that is not at this table", map[uint32]int64{0: stakeAtoms, 9: stakeAtoms}},
	} {
		if _, err := rt.sharesFor(tbl, []uint32{0, 1}, Outcome{Shares: tc.shares}); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

func TestEveryDivisionOfThePotIsExpressible(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	tbl := fakeFundedTable(t, rt, 2)
	pot := int64(stakeAtoms * 2)
	for _, shares := range []map[uint32]int64{
		{0: pot, 1: 0}, {0: 0, 1: pot}, {0: pot / 2, 1: pot / 2}, {0: pot / 4 * 3, 1: pot / 4},
	} {
		got, err := rt.sharesFor(tbl, []uint32{0, 1}, Outcome{Shares: shares})
		if err != nil {
			t.Fatalf("division %v: %v", shares, err)
		}
		if got[0]+got[1] != pot {
			t.Fatalf("division %v does not total %d", got, pot)
		}
	}
}

func TestAVoidTableReturnsEverySeatsOwnStake(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	tbl := fakeFundedTable(t, rt, 2)
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
	for _, tc := range []struct {
		match string
		out   Outcome
	}{
		{"", Outcome{Void: true}},
		{"abcdef01", Outcome{}},
		{"no-such-table", Outcome{Void: true}},
	} {
		if err := rt.Settle(context.Background(), tc.match, tc.out); err == nil {
			t.Errorf("settled invalid table/outcome %q/%+v", tc.match, tc.out)
		}
	}
}

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
	if errors.Is(err, ErrNotYet) {
		t.Fatalf("an unfunded table reported settlement as unbuilt: %v", err)
	}
}

func fakeFundedTable(t *testing.T, rt *Runtime, seats uint32) *table {
	t.Helper()
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	tbl := rt.tables[sid]
	tbl.funded = map[uint32]staked{}
	for seat := range seats {
		tbl.funded[seat] = staked{outpoint: stakeOutpoint(seat), atoms: stakeAtoms}
	}
	return tbl
}

func stakeOutpoint(seat uint32) string {
	return strings.Repeat("cd", 32) + ":" + strconv.Itoa(int(seat))
}

func TestASettlementIsBuiltInSeatOrder(t *testing.T) {
	seats := map[uint32][]byte{3: {3}, 0: {0}, 5: {5}, 1: {1}}
	for range 20 {
		got := seatOrder(seats)
		want := []uint32{0, 1, 3, 5}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}
}

func ourSeatOf(t *testing.T, rt *Runtime, sid string) uint32 {
	t.Helper()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	seat, ok := rt.tables[sid].formation().OurSeat()
	if !ok {
		t.Fatal("we have no seat")
	}
	return seat
}

func mustSeats(t *testing.T, rt *Runtime, sid string) map[uint32][]byte {
	t.Helper()
	seats, ok := rt.Seats(sid)
	if !ok {
		t.Fatal("the table has no seats")
	}
	return seats
}

func TestAWithheldSeatPreventsAPayoutProposal(t *testing.T) {
	game := &withholding{}
	fake, rt, _ := stand(t, game)
	sid, them := seatTwo(t, fake, rt)
	fundBoth(t, fake, rt, sid, them)
	theirSeat, _ := them.form.OurSeat()
	game.hold(theirSeat)

	pot := int64(stakeAtoms * 2)
	winner := ourSeatOf(t, rt, sid)
	shares := map[uint32]int64{winner: pot}
	for seat := range mustSeats(t, rt, sid) {
		if seat != winner {
			shares[seat] = 0
		}
	}
	err := rt.Settle(context.Background(), sid, Outcome{Shares: shares})
	if err == nil || !strings.Contains(err.Error(), "verified") {
		t.Fatalf("withheld payout refusal: %v", err)
	}
	rt.mu.Lock()
	id := rt.tables[sid].payoutID
	rt.mu.Unlock()
	if id != "" {
		t.Fatal("a withheld outcome reached bridge payout approval")
	}
}
