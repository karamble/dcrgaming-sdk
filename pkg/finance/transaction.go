package finance

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
)

// Input is a verified previous output. Bridges obtain its value and script
// from their own node, never from a game-supplied transaction witness.
type Input struct {
	Terms    Terms         `json:"terms"`
	Outpoint wire.OutPoint `json:"outpoint"`
}

// Refund creates an unsigned refund. Chain maturity and destination ownership
// are checked by the bridge before approval and again before signing.
func Refund(input Input, destination []byte, fee int64) (*wire.MsgTx, error) {
	if _, err := input.Terms.Script(); err != nil {
		return nil, err
	}
	if len(destination) == 0 || fee <= 0 || fee >= input.Terms.Atoms {
		return nil, fmt.Errorf("invalid recovery destination or fee")
	}
	if input.Outpoint.Tree != wire.TxTreeRegular {
		return nil, fmt.Errorf("deposit must be a regular-tree output")
	}
	tx := wire.NewMsgTx()
	tx.Version = 3
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: input.Outpoint, ValueIn: input.Terms.Atoms, Sequence: input.Terms.LockBlocks, BlockHeight: wire.NullBlockHeight, BlockIndex: wire.NullBlockIndex})
	tx.AddTxOut(wire.NewTxOut(input.Terms.Atoms-fee, append([]byte(nil), destination...)))
	return tx, nil
}

// SignatureHash binds a wallet signature to the complete financial transaction.
// There is deliberately no configurable hash type.
func SignatureHash(tx *wire.MsgTx, index int, input Input) ([]byte, error) {
	if tx == nil || index < 0 || index >= len(tx.TxIn) || tx.TxIn[index].PreviousOutPoint != input.Outpoint || tx.TxIn[index].ValueIn != input.Terms.Atoms {
		return nil, fmt.Errorf("financial input mismatch")
	}
	script, err := input.Terms.Script()
	if err != nil {
		return nil, err
	}
	return txscript.CalcSignatureHash(script, txscript.SigHashAll, tx, index, nil)
}

// VerifySignature accepts DER ECDSA signatures from the wallet, without a
// appended hash byte. Games cannot select a weaker signature hash mode.
func VerifySignature(tx *wire.MsgTx, index int, input Input, public string, signature []byte) error {
	key, err := PublicKey(public)
	if err != nil {
		return err
	}
	pub, err := secp256k1.ParsePubKey(key)
	if err != nil {
		return err
	}
	sig, err := ecdsa.ParseDERSignature(signature)
	if err != nil {
		return err
	}
	hash, err := SignatureHash(tx, index, input)
	if err != nil {
		return err
	}
	if !sig.Verify(hash, pub) {
		return fmt.Errorf("invalid financial signature")
	}
	return nil
}

func RefundWitness(tx *wire.MsgTx, index int, input Input, signature []byte) ([]byte, error) {
	if err := VerifySignature(tx, index, input, input.Terms.Recovery, signature); err != nil {
		return nil, err
	}
	script, err := input.Terms.Script()
	if err != nil {
		return nil, err
	}
	b := txscript.NewScriptBuilder().AddData(append(append([]byte(nil), signature...), byte(txscript.SigHashAll)))
	if input.Terms.Kind != "seatbond" {
		b.AddOp(txscript.OP_FALSE)
	}
	return b.AddData(script).Script()
}

// SettlementWitness requires a valid signature for every canonical member.
func SettlementWitness(tx *wire.MsgTx, index int, input Input, signatures map[string][]byte) ([]byte, error) {
	terms, err := input.Terms.Canonical()
	if err != nil {
		return nil, err
	}
	if terms.Kind == "seatbond" || len(signatures) != len(terms.Members) {
		return nil, fmt.Errorf("incomplete cooperative signature set")
	}
	script, err := terms.Script()
	if err != nil {
		return nil, err
	}
	b := txscript.NewScriptBuilder()
	for i := len(terms.Members) - 1; i >= 0; i-- {
		member := terms.Members[i]
		sig := signatures[member]
		if err := VerifySignature(tx, index, input, member, sig); err != nil {
			return nil, err
		}
		b.AddData(append(append([]byte(nil), sig...), byte(txscript.SigHashAll)))
	}
	return b.AddOp(txscript.OP_TRUE).AddData(script).Script()
}

func VerifySpend(tx *wire.MsgTx, inputs []Input, params stdaddr.AddressParams) error {
	if tx == nil || len(inputs) == 0 || len(tx.TxIn) != len(inputs) {
		return fmt.Errorf("financial inputs mismatch")
	}
	seen := map[wire.OutPoint]bool{}
	for i, input := range inputs {
		if seen[input.Outpoint] {
			return fmt.Errorf("duplicate financial input")
		}
		seen[input.Outpoint] = true
		if _, err := SignatureHash(tx, i, input); err != nil {
			return err
		}
		_, pk, err := input.Terms.Output(params)
		if err != nil {
			return err
		}
		vm, err := txscript.NewEngine(pk, tx, i, txscript.ScriptVerifyCheckSequenceVerify|txscript.ScriptVerifyCleanStack|txscript.ScriptVerifySigPushOnly, 0, nil)
		if err != nil {
			return err
		}
		if err = vm.Execute(); err != nil {
			return fmt.Errorf("financial input %d: %w", i, err)
		}
	}
	return nil
}

// SameIntent compares prefix and non-signature witness metadata. Decred's
// TxHash alone does not commit to ValueIn or the other witness fields.
func SameIntent(a, b *wire.MsgTx) bool {
	if a == nil || b == nil || a.SerType != wire.TxSerializeFull || b.SerType != wire.TxSerializeFull || a.TxHash() != b.TxHash() || len(a.TxIn) != len(b.TxIn) {
		return false
	}
	for i, in := range a.TxIn {
		other := b.TxIn[i]
		if in.ValueIn != other.ValueIn || in.BlockHeight != other.BlockHeight || in.BlockIndex != other.BlockIndex {
			return false
		}
	}
	return true
}

// DecodeTransaction rejects trailing bytes and non-full serialization.
func DecodeTransaction(raw []byte) (*wire.MsgTx, error) {
	tx := wire.NewMsgTx()
	reader := bytes.NewReader(raw)
	if err := tx.Deserialize(reader); err != nil {
		return nil, err
	}
	if reader.Len() != 0 || tx.SerType != wire.TxSerializeFull {
		return nil, fmt.Errorf("noncanonical financial transaction")
	}
	encoded, err := tx.Bytes()
	if err != nil || !bytes.Equal(encoded, raw) {
		return nil, fmt.Errorf("noncanonical financial bytes")
	}
	return tx, nil
}

func KeyHex(key []byte) (string, error) {
	raw := hex.EncodeToString(key)
	_, err := PublicKey(raw)
	return raw, err
}
