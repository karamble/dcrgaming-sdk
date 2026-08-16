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

// Paying out what a table ended holding.
//
// Everything else in this package moves one player's own coin: a stake back to
// its depositor, a bond back to its owner, a forfeit to the seats that stayed.
// This is the one that moves coin *between* players, which is the whole point of
// having played, and it is the only transaction here that spends every seat's
// deposit at once.
//
// One input per seat, each behind that seat's own redeem script, and each
// needing every member's signature - the settlement branch names the whole
// table, so no seat's stake moves without all of them. One output per seat for
// what the last agreed checkpoint says they hold.
//
// Which checkpoint that is settles everything else. A hand in progress was never
// agreed by anybody, so a table that stops pays out at its last boundary and
// voids what was under way. Nobody has to work out who stopped it: the
// transaction is the same whoever assembles it.

// SettleInput is one seat's stake, and the script it sits behind.
type SettleInput struct {
	// Redeem is that seat's own escrow script. Every seat has a different
	// one - the refund branch names only its owner - so a settlement cannot
	// be signed with one script for the lot.
	Redeem     []byte
	Prevout    wire.OutPoint
	ValueAtoms int64
}

// SettleDraft is a table's final payout.
//
// Inputs and Pays are parallel and both in seat order, because that is what
// decides the transaction's bytes and every peer has to build the same ones.
// Amounts comes from the checkpoint every seat signed, which is what makes this
// a payout everybody already agreed rather than one somebody proposes.
type SettleDraft struct {
	Inputs   []SettleInput
	Pays     [][]byte
	Amounts  []int64
	FeeAtoms int64
}

// BuildSettlement builds the unsigned transaction that pays a table out.
//
// The fee falls on the seats evenly with the remainder on the first, so nobody
// underwrites anybody else's exit - the same rule the abort transaction uses,
// and it has to be a rule rather than a preference because two peers rounding
// differently produce signatures that cannot be combined.
//
// A seat holding nothing at the end gets no output at all. Paying somebody zero
// makes a transaction the network will not relay, and a busted player is not
// owed a line in it.
func BuildSettlement(d SettleDraft) (*wire.MsgTx, error) {
	n := len(d.Inputs)
	switch {
	case n < 2:
		return nil, fmt.Errorf("a settlement spends every seat's stake; this has %d", n)
	case n > MaxMembers:
		return nil, fmt.Errorf("%d seats exceeds the maximum of %d", n, MaxMembers)
	case len(d.Pays) != n || len(d.Amounts) != n:
		return nil, fmt.Errorf("%d seats but %d payouts and %d amounts",
			n, len(d.Pays), len(d.Amounts))
	case d.FeeAtoms < 0:
		return nil, fmt.Errorf("a negative fee")
	}

	var held, staked int64
	for i, in := range d.Inputs {
		if len(in.Redeem) == 0 {
			return nil, fmt.Errorf("seat %d has no redeem script", i)
		}
		if in.ValueAtoms <= 0 {
			return nil, fmt.Errorf("seat %d's stake holds nothing", i)
		}
		staked += in.ValueAtoms
		if d.Amounts[i] < 0 {
			return nil, fmt.Errorf("seat %d is owed %d", i, d.Amounts[i])
		}
		held += d.Amounts[i]
	}
	// Nothing may be created or destroyed. A settlement paying out more than
	// the table holds is one the network refuses; one paying out less quietly
	// gives the difference to a miner.
	if held != staked {
		return nil, fmt.Errorf("the seats hold %d between them and the stakes are %d",
			held, staked)
	}

	// The fee, on the seats that still have something to pay it from.
	paying := 0
	for _, a := range d.Amounts {
		if a > 0 {
			paying++
		}
	}
	if paying == 0 {
		return nil, fmt.Errorf("every seat ended with nothing")
	}
	share, rem := d.FeeAtoms/int64(paying), d.FeeAtoms%int64(paying)

	tx := wire.NewMsgTx()
	// Version 3, because OP_CHECKSEQUENCEVERIFY refuses anything under 2 and
	// everything else here is written at 3.
	tx.Version = 3
	for _, in := range d.Inputs {
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: in.Prevout,
			ValueIn:          in.ValueAtoms,
			// The settlement branch carries no timelock: it is what
			// everybody agreeing looks like, and agreement does not have
			// to wait.
			Sequence: 0,
		})
	}
	first := true
	for i, amount := range d.Amounts {
		if amount == 0 {
			continue
		}
		owed := amount - share
		if first {
			owed -= rem
			first = false
		}
		if owed < MinShareAtoms {
			return nil, fmt.Errorf("seat %d would be paid %d, under the %d minimum",
				i, owed, MinShareAtoms)
		}
		if len(d.Pays[i]) == 0 {
			return nil, fmt.Errorf("seat %d has nowhere to be paid", i)
		}
		tx.AddTxOut(wire.NewTxOut(owed, d.Pays[i]))
	}
	return tx, nil
}

// SignSettlement produces one member's signature over every input.
//
// Every input, because the settlement branch of each seat's script names the
// whole table: a member signs its neighbours' stakes as well as its own, and a
// settlement short of one signature on one input is short of the lot.
func SignSettlement(tx *wire.MsgTx, d SettleDraft, key *secp256k1.PrivateKey) ([][]byte, error) {
	if tx == nil {
		return nil, fmt.Errorf("no transaction to sign")
	}
	if key == nil {
		return nil, fmt.Errorf("no signing key")
	}
	if len(tx.TxIn) != len(d.Inputs) {
		return nil, fmt.Errorf("the transaction has %d inputs and the draft has %d",
			len(tx.TxIn), len(d.Inputs))
	}
	out := make([][]byte, 0, len(d.Inputs))
	for i, in := range d.Inputs {
		sighash, err := txscript.CalcSignatureHash(in.Redeem, txscript.SigHashAll, tx, i, nil)
		if err != nil {
			return nil, fmt.Errorf("input %d signature hash: %w", i, err)
		}
		sig, err := schnorr.Sign(key, sighash)
		if err != nil {
			return nil, fmt.Errorf("input %d: %w", i, err)
		}
		out = append(out, append(sig.Serialize(), byte(txscript.SigHashAll)))
	}
	return out, nil
}

// FinishSettlement attaches every member's signatures and checks the result
// against the real script engine.
//
// sigs is indexed by input and then by member, in the canonical member order the
// redeem scripts check them in. Running the engine here turns a missing or
// misplaced signature into something a person can read, rather than a
// transaction the network rejects for a reason nothing in it points at.
func FinishSettlement(tx *wire.MsgTx, d SettleDraft, sigs [][][]byte, params stdaddr.AddressParams) (*wire.MsgTx, error) {
	if tx == nil || len(tx.TxIn) != len(d.Inputs) {
		return nil, fmt.Errorf("the transaction does not match the draft")
	}
	if len(sigs) != len(d.Inputs) {
		return nil, fmt.Errorf("%d inputs but signatures for %d", len(d.Inputs), len(sigs))
	}
	out := tx.Copy()
	for i, in := range d.Inputs {
		sigScript, err := SettlementSigScript(in.Redeem, sigs[i])
		if err != nil {
			return nil, fmt.Errorf("input %d: %w", i, err)
		}
		out.TxIn[i].SignatureScript = sigScript
	}
	for i, in := range d.Inputs {
		_, pkScript, err := Address(in.Redeem, params)
		if err != nil {
			return nil, fmt.Errorf("input %d: %w", i, err)
		}
		vm, err := txscript.NewEngine(pkScript, out, i,
			txscript.ScriptVerifyCheckSequenceVerify, scriptVersion, nil)
		if err != nil {
			return nil, fmt.Errorf("input %d: %w", i, err)
		}
		if err := vm.Execute(); err != nil {
			return nil, fmt.Errorf("input %d does not satisfy its escrow: %w", i, err)
		}
	}
	return out, nil
}

// CheckSettleDraft is what a seat runs before signing somebody else's
// settlement.
//
// Signing one without looking is how a player authorises the table's whole
// balance being paid somewhere else. What is checked is that it is the
// transaction this peer would have built from the checkpoint it holds - which
// covers the amounts, the addresses and the inputs at once, and needs no
// judgement about any of them.
func CheckSettleDraft(tx *wire.MsgTx, d SettleDraft) error {
	want, err := BuildSettlement(d)
	if err != nil {
		return err
	}
	if tx == nil {
		return fmt.Errorf("no transaction")
	}
	if tx.Version != want.Version {
		return fmt.Errorf("settlement is version %d, want %d", tx.Version, want.Version)
	}
	if len(tx.TxIn) != len(want.TxIn) || len(tx.TxOut) != len(want.TxOut) {
		return fmt.Errorf("a settlement here has %d inputs and %d outputs; this has %d and %d",
			len(want.TxIn), len(want.TxOut), len(tx.TxIn), len(tx.TxOut))
	}
	for i := range want.TxIn {
		if tx.TxIn[i].PreviousOutPoint != want.TxIn[i].PreviousOutPoint {
			return fmt.Errorf("input %d spends a different stake", i)
		}
		if tx.TxIn[i].ValueIn != want.TxIn[i].ValueIn {
			return fmt.Errorf("input %d states a stake of %d, not %d",
				i, tx.TxIn[i].ValueIn, want.TxIn[i].ValueIn)
		}
		if tx.TxIn[i].Sequence != want.TxIn[i].Sequence {
			return fmt.Errorf("input %d carries a sequence of %d", i, tx.TxIn[i].Sequence)
		}
	}
	for i := range want.TxOut {
		if tx.TxOut[i].Value != want.TxOut[i].Value {
			return fmt.Errorf("output %d pays %d, and the checkpoint says %d",
				i, tx.TxOut[i].Value, want.TxOut[i].Value)
		}
		if !bytes.Equal(tx.TxOut[i].PkScript, want.TxOut[i].PkScript) {
			return fmt.Errorf("output %d pays somewhere this peer did not agree", i)
		}
	}
	return nil
}
