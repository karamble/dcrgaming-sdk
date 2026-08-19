package punish

import (
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
)

// The liveness ladder at two seats (spec 8.3). The accuse chain is built and
// co-signed at bonding, while the table still cooperates; the single-sig
// answer is how a present seat keeps its bond; the take is what an unanswered
// window costs, and like the sweep it can only pay the pinned payout.

// LadderDepth is how many accusations a bond of this size affords at a given
// fee, with the single taker a heads-up table has.
//
// The fee is a parameter rather than a constant because it is a game's economic
// choice, and the whole attrition bound moves with it. Worked through for
// dcrbattleships' gv1 numbers (bond 1,000,000 atoms, fee 10,000, one taker):
// room after the taker's minimum share is 1,000,000 - 20,000 = 980,000, each
// rung costs two fees = 20,000, so 49 rungs are affordable and the agreed cap
// escrow.AccuseDepth = 8 binds instead. A full ladder therefore burns at most
// 2*8*10,000 = 160,000 atoms of the accused bond.
func LadderDepth(bondAtoms, feeAtoms int64) int {
	return escrow.AffordableDepth(bondAtoms, feeAtoms, 1)
}

// BuildLadder builds every accusation the bond affords, in rung order, for a
// two-seat table. Nothing is signed here: both seats co-sign each rung at
// bonding, and the chain's later outpoints are already fixed because a Decred
// transaction's identity excludes its witness.
func BuildLadder(d escrow.AccuseDraft) ([]*wire.MsgTx, error) {
	if _, err := twoSeats(d.Bond); err != nil {
		return nil, err
	}
	// Every rung is built at the draft's own fee, and AttritionBound is
	// computed from the same number, so a caller that changes one changes
	// both. A non-positive fee would build a chain the network will not
	// relay.
	if d.FeeAtoms <= 0 {
		return nil, fmt.Errorf("an accusation chain needs a fee, and this one is %d", d.FeeAtoms)
	}
	depth := LadderDepth(d.ValueAtoms, d.FeeAtoms)
	if depth < 1 {
		return nil, fmt.Errorf("a bond of %d affords no accusation", d.ValueAtoms)
	}
	return escrow.BuildAccuseChain(d, depth)
}

// CoSignAccuse finishes one rung with both seats' signatures in canonical
// member order. The result is the co-signed shape: two signatures on its one
// input, which is what lets the bridge relay it anywhere.
func CoSignAccuse(tx *wire.MsgTx, bond []byte, sigs [][]byte, params stdaddr.AddressParams) (*wire.MsgTx, error) {
	if _, err := twoSeats(bond); err != nil {
		return nil, err
	}
	return escrow.FinishAlive(tx, bond, sigs, params)
}

// AnswerClaim finishes the accused seat's reply, signed alone: the claimed
// bond straight back into its own table bond. Single-sig and value-conserving
// but for the rung fee, so it is the one punishment-path spend that goes out
// through the bridge's passing-through shape.
func AnswerClaim(key *secp256k1.PrivateKey, d escrow.AnswerDraft) (*wire.MsgTx, error) {
	tx, err := escrow.BuildAnswer(d)
	if err != nil {
		return nil, err
	}
	sig, err := escrow.SignClaimedSpend(tx, d.Claimed, key)
	if err != nil {
		return nil, err
	}
	sigScript, err := escrow.AnswerSigScript(d.Claimed, sig)
	if err != nil {
		return nil, err
	}
	return finishClaimedSpend(tx, d.Claimed, sigScript, d.Params)
}

// Take describes spending a claimed bond whose answer window has closed.
// As with Sweep, the destination is not in here: the pinned payout is a
// separate argument to TakeExpired.
type Take struct {
	// Claimed is the claimed bond script the unanswered rung paid into.
	Claimed    []byte
	Prevout    wire.OutPoint
	ValueAtoms int64
	FeeAtoms   int64
	Params     stdaddr.AddressParams
}

// TakeExpired spends an expired claimed bond, entire, to the pinned payout,
// signed by the wronged seat alone - heads-up there is exactly one taker. The
// input carries Sequence = ClaimBlocks, so the engine itself refuses a take
// built before the window could have closed.
func TakeExpired(key *secp256k1.PrivateKey, pinned []byte, t Take) (*wire.MsgTx, error) {
	if len(pinned) == 0 {
		return nil, fmt.Errorf("no pinned payout to take to")
	}
	terms, err := escrow.ParseClaimedBond(t.Claimed)
	if err != nil {
		return nil, err
	}
	if len(terms.Others) != 1 {
		return nil, fmt.Errorf("the gv1 ladder runs at two seats; this claimed bond pays %d",
			len(terms.Others))
	}
	tx, err := escrow.BuildTake(escrow.TakeDraft{
		Claimed:    t.Claimed,
		Prevout:    t.Prevout,
		ValueAtoms: t.ValueAtoms,
		PayScripts: [][]byte{pinned},
		FeeAtoms:   t.FeeAtoms,
	})
	if err != nil {
		return nil, err
	}
	sig, err := escrow.SignClaimedSpend(tx, t.Claimed, key)
	if err != nil {
		return nil, err
	}
	sigScript, err := escrow.TakeSigScript(t.Claimed, [][]byte{sig})
	if err != nil {
		return nil, err
	}
	final, err := finishClaimedSpend(tx, t.Claimed, sigScript, t.Params)
	if err != nil {
		return nil, err
	}
	if err := CheckPinned(final, pinned); err != nil {
		return nil, err
	}
	return final, nil
}

// twoSeats parses a table bond and holds it to the pilot's shape.
func twoSeats(bond []byte) (*escrow.TableBondTerms, error) {
	terms, err := escrow.ParseTableBond(bond)
	if err != nil {
		return nil, err
	}
	if len(terms.Members) != 2 {
		return nil, fmt.Errorf("the gv1 ladder runs at two seats, not %d", len(terms.Members))
	}
	return terms, nil
}

// finishClaimedSpend attaches the witness and runs the spend through the real
// script engine before handing it back, the way the SDK finishes bond spends.
func finishClaimedSpend(tx *wire.MsgTx, claimed, sigScript []byte, params stdaddr.AddressParams) (*wire.MsgTx, error) {
	if tx == nil || len(tx.TxIn) != 1 {
		return nil, fmt.Errorf("a claimed-bond spend has exactly one input")
	}
	out := tx.Copy()
	out.TxIn[0].SignatureScript = sigScript

	_, pkScript, err := escrow.Address(claimed, params)
	if err != nil {
		return nil, fmt.Errorf("derive the claimed bond's address: %w", err)
	}
	vm, err := txscript.NewEngine(pkScript, out, 0,
		txscript.ScriptVerifyCheckSequenceVerify, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("build the script engine: %w", err)
	}
	if err := vm.Execute(); err != nil {
		return nil, fmt.Errorf("the spend does not satisfy the claimed bond: %w", err)
	}
	return out, nil
}
