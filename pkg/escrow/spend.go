package escrow

import (
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
)

// Taking coin back out of a timelocked script.
//
// Two things here do that: a depositor reclaiming their own stake through the
// escrow's refund branch, and a player reclaiming their fidelity bond. They are
// the same transaction. What differs is which script the signature commits to
// and how the witness selects a branch - the escrow has two and needs one
// chosen, the bond has one and needs nothing said. So it is written once, with
// those two as arguments.
//
// It lives beside the scripts rather than in whatever binary happens to need
// it, for the same reason the scripts do: this is where a one-byte disagreement
// costs somebody their money, and where the tests that drive the real consensus
// engine already are.

// Spend describes taking coin out of a timelocked script.
type Spend struct {
	// Key signs. It has to be the key the script names; anything else
	// produces a transaction the network simply refuses.
	Key *secp256k1.PrivateKey

	// Script is the redeem or bond script. It is what the signature commits
	// to - not the P2SH pkScript that pays it, which is only its hash.
	Script []byte

	// Prevout and ValueAtoms identify the coin. The value is not
	// bookkeeping: Decred's signature hash commits to it, so a wrong one
	// yields a signature that verifies against nothing and a transaction
	// that fails for a reason nothing in it points at.
	Prevout    wire.OutPoint
	ValueAtoms int64

	// CSVBlocks is the lock the script carries. It goes into the input's
	// sequence, which is the only way the branch is satisfied at all.
	CSVBlocks uint32

	// PayScript is where the coin goes; FeeAtoms is what is left behind for
	// the miner. There is one output and no change - this spends a single
	// deposit in full.
	PayScript []byte
	FeeAtoms  int64

	// SigScript assembles the witness for the branch being taken:
	// RefundSigScript selects the escrow's OP_ELSE, BondSigScript has no
	// branch to select.
	SigScript func(script, sig []byte) ([]byte, error)

	// Params is needed only to rebuild the pkScript this spend must satisfy,
	// so the result can be checked before anybody broadcasts it.
	Params stdaddr.AddressParams
}

// BuildTimelockedSpend signs and returns the transaction, having first checked
// it against the real script engine.
//
// What that check does and does not establish is worth being exact about. It
// runs with the sequence rule enabled, so it proves the witness satisfies the
// branch and that the sequence is at least the lock the script names. It cannot
// prove the coin is old enough - maturity is a property of how many blocks are
// on top of the output, which no engine can see from the transaction alone. The
// caller has to establish that separately, or the network will reject a
// transaction that verified perfectly here.
func BuildTimelockedSpend(s Spend) (*wire.MsgTx, error) {
	switch {
	case s.Key == nil:
		return nil, fmt.Errorf("no signing key")
	case len(s.Script) == 0:
		return nil, fmt.Errorf("no script to spend")
	case len(s.PayScript) == 0:
		return nil, fmt.Errorf("nowhere to pay the coin")
	case s.SigScript == nil:
		return nil, fmt.Errorf("no witness builder for the branch being taken")
	case s.CSVBlocks == 0:
		return nil, fmt.Errorf("no timelock, so no branch to satisfy")
	case s.CSVBlocks > MaxCSVBlocks:
		return nil, fmt.Errorf("timelock %d is beyond %d and could never be spent",
			s.CSVBlocks, MaxCSVBlocks)
	case s.ValueAtoms <= 0:
		return nil, fmt.Errorf("the output being spent holds nothing")
	}
	payout := s.ValueAtoms - s.FeeAtoms
	if payout <= 0 {
		return nil, fmt.Errorf("a fee of %d leaves nothing of %d", s.FeeAtoms, s.ValueAtoms)
	}

	tx := wire.NewMsgTx()
	// Version 2 or later, because OP_CHECKSEQUENCEVERIFY refuses anything
	// earlier (wire.TxVersionSeqLock). Not, as a couple of comments in this
	// repository claim, because of Schnorr: OP_CHECKSIGALTVERIFY has no
	// version gate at all. Three, to match everything already written.
	tx.Version = 3
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: s.Prevout,
		ValueIn:          s.ValueAtoms,
		Sequence:         s.CSVBlocks,
	})
	tx.AddTxOut(wire.NewTxOut(payout, s.PayScript))

	// Everything above is covered by SigHashAll, so from here only the
	// signature script may change.
	sighash, err := txscript.CalcSignatureHash(s.Script, txscript.SigHashAll, tx, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("signature hash: %w", err)
	}
	sig, err := schnorr.Sign(s.Key, sighash)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	sigScript, err := s.SigScript(s.Script, append(sig.Serialize(), byte(txscript.SigHashAll)))
	if err != nil {
		return nil, err
	}
	tx.TxIn[0].SignatureScript = sigScript

	_, pkScript, err := Address(s.Script, s.Params)
	if err != nil {
		return nil, fmt.Errorf("derive the script's address: %w", err)
	}
	vm, err := txscript.NewEngine(pkScript, tx, 0,
		txscript.ScriptVerifyCheckSequenceVerify, scriptVersion, nil)
	if err != nil {
		return nil, fmt.Errorf("build the script engine: %w", err)
	}
	if err := vm.Execute(); err != nil {
		return nil, fmt.Errorf("the spend does not satisfy the script: %w", err)
	}
	return tx, nil
}
