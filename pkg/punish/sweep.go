// Package punish turns engine verdicts and retained evidence into finished,
// broadcast-ready spends. It is pure orchestration of the SDK's escrow and
// forfeit primitives: it builds and inspects transactions and does no chain or
// network IO; broadcasting is the daemon's milestone. Every spend built here
// is single-output to a caller-supplied bridge-pinned payout script, and the
// builders keep any other destination unrepresentable (spec 9.5). The one
// exception is the clean-end release, which takes nothing and pays its owner.
//
// The escrow.AffordableDepth table at the gv1 constants (AccuseFeeAtoms
// 10_000, one taker):
//
//	bond atoms   depth
//	 1_000_000   8   the gv1 TableBondAtoms; the arithmetic affords
//	                 49 rungs and the agreed cap of 8 binds instead
//	   180_000   8   the smallest bond still capped
//	   160_000   7
//	    60_000   2
//	    40_000   1
//	    20_000   0   one minimum share; nothing left to accuse over
//
// The full ladder therefore burns at most 2*8*10_000 = 160_000 atoms of the
// accused bond - the documented gv1 attrition bound a live refuser pays.
package punish

import (
	"bytes"
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
)

// Sweep describes the one spend an equivocation permits: the cheat's
// forfeitable bond, entire, through the victim's punishment branch. Where it
// pays is not in here - the pinned payout is a separate argument to
// SweepForfeited, so a draft cannot carry its own destination.
type Sweep struct {
	// Bond is the cheat's forfeitable bond redeem script.
	Bond       []byte
	Prevout    wire.OutPoint
	ValueAtoms int64
	FeeAtoms   int64

	// Branch names the victim's punishment branch in that bond, and Punisher
	// is the victim's own half of the branch key.
	Branch   forfeit.Branch
	Punisher *secp256k1.PrivateKey

	Params stdaddr.AddressParams
}

// SweepForfeited turns a recovered log key - the evidence store's finding on a
// divergent pair of attestations - into the finished punishment-branch spend,
// verified by the real script engine on the way out.
//
// Nothing here takes the caller's word that the key belongs to the seat being
// punished, and nothing needs to. The key is aggregated with the punisher half
// and looked for among the bond's branches: a key that is not the accused's
// yields a point that is in none of them, and escrow.ForfeitIndex refuses it
// before a transaction is built. That refusal, and the script engine behind
// it, are the whole authorisation - so it holds only while the bond handed in
// was derived from the roster rather than believed off the wire.
//
// The single output is the pinned payout and nothing else can be expressed:
// the draft has no destination field, BuildForfeit cannot add a second output,
// and CheckPinned holds the finished bytes to the script the caller supplied.
func SweepForfeited(recovered *secp256k1.PrivateKey, pinned []byte, s Sweep) (*wire.MsgTx, error) {
	if recovered == nil {
		return nil, fmt.Errorf("no recovered key; nothing was forfeited")
	}
	if s.Punisher == nil {
		return nil, fmt.Errorf("no punisher half; this branch is not ours to spend")
	}
	if len(pinned) == 0 {
		return nil, fmt.Errorf("no pinned payout to sweep to")
	}
	spendKey, err := forfeit.ForfeitPrivKey(s.Branch, recovered, s.Punisher)
	if err != nil {
		return nil, fmt.Errorf("assemble the branch secret: %w", err)
	}
	terms, err := escrow.ParseForfeitableBond(s.Bond)
	if err != nil {
		return nil, err
	}
	index, err := escrow.ForfeitIndex(terms, spendKey.PubKey().SerializeCompressed())
	if err != nil {
		return nil, err
	}
	tx, err := escrow.BuildForfeit(escrow.ForfeitDraft{
		Bond:       s.Bond,
		Prevout:    s.Prevout,
		ValueAtoms: s.ValueAtoms,
		PayScript:  pinned,
		FeeAtoms:   s.FeeAtoms,
	})
	if err != nil {
		return nil, err
	}
	sig, err := escrow.SignForfeitableSpend(tx, s.Bond, spendKey)
	if err != nil {
		return nil, err
	}
	final, err := escrow.FinishForfeit(tx, s.Bond, sig, index, s.Params)
	if err != nil {
		return nil, err
	}
	if err := CheckPinned(final, pinned); err != nil {
		return nil, err
	}
	return final, nil
}

// CheckPinned refuses any punishment spend that is not single-output to the
// pinned payout. The builders run it on their own results, and the daemon runs
// it again before handing anything to the bridge (spec 9.5): these spends are
// unilateral, so their only legal shape is everything to the pinned script.
func CheckPinned(tx *wire.MsgTx, pinned []byte) error {
	if len(pinned) == 0 {
		return fmt.Errorf("no pinned payout to hold the spend to")
	}
	if tx == nil || len(tx.TxOut) != 1 {
		return fmt.Errorf("a punishment spend has exactly one output")
	}
	if !bytes.Equal(tx.TxOut[0].PkScript, pinned) {
		return fmt.Errorf("the spend pays somewhere other than the pinned payout")
	}
	return nil
}
