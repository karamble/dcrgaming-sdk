package punish

import (
	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
)

// The gv1 residual (spec 2, 8.4): a refuser who stays live on chain answers
// every accusation, so no take ever matures and no lever remains. What the
// ladder extracts is fees alone - the pot voids, the stakes CSV-refund, and
// neither seat won anything. Every surface reports it exactly that way; the
// adaptor-signature settlement that closes it is gv2.

// Attrition is the typed result of a ladder answered to its end. It is a
// residual and never a win: the refuser keeps the bond less the fees, and the
// match voids.
type Attrition struct {
	// RungsRun is how many accusations were placed and answered.
	RungsRun int
	// ExtractedAtoms is the fee total the ladder took out of the bond.
	ExtractedAtoms int64
}

// AttritionBound is the most a full ladder can cost a live refuser: two fees
// per rung, one on the accusation and one on the answer, across the agreed
// depth. At dcrbattleships' gv1 fee of 10,000 atoms that is 160,000 atoms of a
// 1,000,000 bond.
//
// This is what a game tells a player before they bond: it is the worst a seat
// that answers everything honestly can still be made to pay.
func AttritionBound(feeAtoms int64) int64 {
	return 2 * escrow.AccuseDepth * feeAtoms
}
