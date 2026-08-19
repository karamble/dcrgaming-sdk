package punish

import "testing"

// The bound is what a game tells a player before they bond: the worst a seat
// that answers everything honestly can still be made to pay.
//
// Pinned at 10,000 atoms - dcrbattleships' gv1 fee - as a literal, so the
// number is a fact about this code rather than a restatement of a constant it
// is supposed to be checking.
func TestTheAttritionBoundIsTwoFeesPerRung(t *testing.T) {
	const fee = 10_000
	if got, want := AttritionBound(fee), int64(160_000); got != want {
		t.Fatalf("AttritionBound(%d) = %d, want %d", fee, got, want)
	}
}

// It moves with the fee, which is the point of the fee being a parameter.
func TestTheAttritionBoundFollowsTheFee(t *testing.T) {
	if a, b := AttritionBound(10_000), AttritionBound(20_000); b != 2*a {
		t.Fatalf("doubling the fee gave %d and %d", a, b)
	}
}
