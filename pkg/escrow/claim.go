package escrow

import (
	"bytes"
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
)

// Spending a table bond, when more than one person has to agree.
//
// The backstop needs one signature and goes through BuildTimelockedSpend like
// any other timelocked reclaim. The other two branches need several, and that
// changes the problem: signatures are made over the transaction, so every
// co-signer has to build *byte for byte the same transaction* before signing
// anything. A builder that produced almost the same bytes on two machines would
// collect signatures that cannot be combined, and the failure would look like a
// disagreement about the facts rather than a rounding difference.
//
// So nothing here takes a number the caller could compute differently. Outputs
// are ordered by the bond's own canonical member order, the split is exact
// division with the remainder placed by rule, and the whole thing is run
// through the real script engine before it is handed back.

// MinShareAtoms is the least a claim may pay one seat.
//
// A share below the network's dust threshold makes the whole transaction
// unrelayable, so a bond small enough to be split into dust cannot be claimed at
// all - which would quietly turn "abandonment is punished" into "abandonment is
// punished at some table sizes". Better to refuse to build it and say so.
const MinShareAtoms int64 = 20_000

// Shares divides a claimed bond among the seats taking it.
//
// Exact division, with the remainder given one atom at a time to the earliest
// seats in canonical order. Any rule would do so long as every peer applies the
// same one; what would not do is dropping the remainder into the fee, which
// looks tidy and makes the amount depend on how the caller rounded.
func Shares(total int64, n int) ([]int64, error) {
	if n <= 0 {
		return nil, fmt.Errorf("nobody to divide %d atoms between", total)
	}
	if total <= 0 {
		return nil, fmt.Errorf("there is nothing to divide")
	}
	base, rem := total/int64(n), total%int64(n)
	out := make([]int64, n)
	for i := range out {
		out[i] = base
		if int64(i) < rem {
			out[i]++
		}
	}
	if out[n-1] < MinShareAtoms {
		return nil, fmt.Errorf("dividing %d atoms between %d seats leaves %d each, "+
			"under the %d minimum", total, n, out[n-1], MinShareAtoms)
	}
	return out, nil
}

type AliveDraft struct {
	Bond       []byte
	Prevout    wire.OutPoint
	ValueAtoms int64
	PayScript  []byte
	FeeAtoms   int64
}

// BuildAlive builds the unsigned transaction that answers a claim or releases a
// bond.
//
// It carries no timelock, and that is the mechanism rather than an omission: an
// answer confirms while a claim on the same output is still waiting out its
// delay, so coming back always beats being accused.
func BuildAlive(d AliveDraft) (*wire.MsgTx, error) {
	if _, err := ParseTableBond(d.Bond); err != nil {
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

// SignBondSpend produces one member's signature over a bond spend.
//
// The same call serves both branches. What a signature commits to is the
// transaction and the script, not which branch will be selected, so the
// selection happens when the witness is assembled and not here.
func SignBondSpend(tx *wire.MsgTx, bond []byte, key *secp256k1.PrivateKey) ([]byte, error) {
	if tx == nil {
		return nil, fmt.Errorf("no transaction to sign")
	}
	if key == nil {
		return nil, fmt.Errorf("no signing key")
	}
	if _, err := ParseTableBond(bond); err != nil {
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

func FinishAlive(tx *wire.MsgTx, bond []byte, sigs [][]byte, params stdaddr.AddressParams) (*wire.MsgTx, error) {
	sigScript, err := AliveSigScript(bond, sigs)
	if err != nil {
		return nil, err
	}
	return finishBondSpend(tx, bond, sigScript, params)
}

func finishBondSpend(tx *wire.MsgTx, bond, sigScript []byte, params stdaddr.AddressParams) (*wire.MsgTx, error) {
	if tx == nil || len(tx.TxIn) != 1 {
		return nil, fmt.Errorf("a bond spend has exactly one input")
	}
	out := tx.Copy()
	out.TxIn[0].SignatureScript = sigScript

	_, pkScript, err := Address(bond, params)
	if err != nil {
		return nil, fmt.Errorf("derive the bond's address: %w", err)
	}
	vm, err := txscript.NewEngine(pkScript, out, 0,
		txscript.ScriptVerifyCheckSequenceVerify, scriptVersion, nil)
	if err != nil {
		return nil, fmt.Errorf("build the script engine: %w", err)
	}
	if err := vm.Execute(); err != nil {
		return nil, fmt.Errorf("the spend does not satisfy the bond script: %w", err)
	}
	return out, nil
}

const AccuseDepth = 8

func CheckAliveDraft(tx *wire.MsgTx, d AliveDraft) error {
	want, err := BuildAlive(d)
	if err != nil {
		return err
	}
	if tx == nil || len(tx.TxIn) != 1 || len(tx.TxOut) != 1 {
		return fmt.Errorf("a bond release has one input and one output")
	}
	if tx.Version != want.Version {
		return fmt.Errorf("release is version %d, want %d", tx.Version, want.Version)
	}
	if tx.TxIn[0].PreviousOutPoint != want.TxIn[0].PreviousOutPoint {
		return fmt.Errorf("release spends a different output than the bond named")
	}
	if tx.TxIn[0].ValueIn != want.TxIn[0].ValueIn {
		return fmt.Errorf("release states the bond holds %d, not %d",
			tx.TxIn[0].ValueIn, want.TxIn[0].ValueIn)
	}
	if tx.TxIn[0].Sequence != want.TxIn[0].Sequence {
		return fmt.Errorf("release carries a sequence of %d, and the branch every member signs has no timelock",
			tx.TxIn[0].Sequence)
	}
	if tx.TxOut[0].Value != want.TxOut[0].Value {
		return fmt.Errorf("release pays out %d, and the fee leaves %d",
			tx.TxOut[0].Value, want.TxOut[0].Value)
	}
	if !bytes.Equal(tx.TxOut[0].PkScript, want.TxOut[0].PkScript) {
		return fmt.Errorf("release pays somewhere other than where this seat asked to be paid")
	}
	return nil
}
