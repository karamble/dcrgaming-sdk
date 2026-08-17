package escrow

import (
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
)

// Spending a forfeitable bond through its punishment branch. The alive/take
// machinery above cannot do it: SignBondSpend validates against ParseTableBond
// and refuses these scripts outright, so until here nothing could produce the
// one signature ForfeitSigScript demands.

// ForfeitDraft describes the one spend a forfeited bond allows: everything to
// one destination the caller has already pinned.
type ForfeitDraft struct {
	Bond       []byte // the cheat's forfeitable bond redeem script
	Prevout    wire.OutPoint
	ValueAtoms int64
	PayScript  []byte // the single output's script; a second output is unrepresentable
	FeeAtoms   int64
}

// BuildForfeit builds the unsigned transaction that takes a forfeited bond
// through a punishment branch.
//
// One input with no timelock - the punishment branch has no sequence to wait
// out - and exactly one output. The single-output shape is deliberate: a
// forfeit spend is unilateral, so where the coin may go is a policy question
// for whoever broadcasts it, and this builder keeps every answer other than
// the caller's pinned destination unrepresentable.
func BuildForfeit(d ForfeitDraft) (*wire.MsgTx, error) {
	if _, err := ParseForfeitableBond(d.Bond); err != nil {
		return nil, err
	}
	if len(d.PayScript) == 0 {
		return nil, fmt.Errorf("nowhere to pay the bond")
	}
	if d.ValueAtoms <= 0 {
		return nil, fmt.Errorf("the bond holds nothing")
	}
	if d.FeeAtoms < 0 {
		return nil, fmt.Errorf("a negative fee")
	}
	payout := d.ValueAtoms - d.FeeAtoms
	if payout < MinShareAtoms {
		return nil, fmt.Errorf("a fee of %d leaves %d of %d, under the %d minimum",
			d.FeeAtoms, payout, d.ValueAtoms, MinShareAtoms)
	}

	tx := wire.NewMsgTx()
	tx.Version = 3
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: d.Prevout,
		ValueIn:          d.ValueAtoms,
		Sequence:         0,
	})
	tx.AddTxOut(wire.NewTxOut(payout, d.PayScript))
	return tx, nil
}

// SignForfeitableSpend produces the one signature a forfeitable bond spend
// carries, for either branch: what a signature commits to is the transaction
// and the script, not which branch the witness will select.
func SignForfeitableSpend(tx *wire.MsgTx, bond []byte, key *secp256k1.PrivateKey) ([]byte, error) {
	if tx == nil {
		return nil, fmt.Errorf("no transaction to sign")
	}
	if key == nil {
		return nil, fmt.Errorf("no signing key")
	}
	if _, err := ParseForfeitableBond(bond); err != nil {
		return nil, err
	}
	sighash, err := txscript.CalcSignatureHash(bond, txscript.SigHashAll, tx, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("signature hash: %w", err)
	}
	sig, err := schnorr.Sign(key, sighash)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	return append(sig.Serialize(), byte(txscript.SigHashAll)), nil
}

// FinishForfeit assembles the punishment-branch witness and runs the spend
// through the real script engine before handing it back, exactly as the other
// bond spends are finished.
func FinishForfeit(tx *wire.MsgTx, bond, sig []byte, index int, params stdaddr.AddressParams) (*wire.MsgTx, error) {
	// The builder cannot express a second output; refuse one smuggled in after.
	if tx == nil || len(tx.TxOut) != 1 {
		return nil, fmt.Errorf("a forfeit spend has exactly one output")
	}
	sigScript, err := ForfeitSigScript(bond, sig, index)
	if err != nil {
		return nil, err
	}
	return finishBondSpend(tx, bond, sigScript, params)
}
