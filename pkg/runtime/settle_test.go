package runtime

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
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
	if !errors.Is(err, ErrNotYet) && !strings.Contains(err.Error(), "seating") {
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
