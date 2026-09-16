package finance

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/wire"
)

func fixture(t *testing.T, kind string) (Input, []*secp256k1.PrivateKey) {
	t.Helper()
	keys := make([]*secp256k1.PrivateKey, 3)
	for i := range keys {
		raw := make([]byte, 32)
		raw[31] = byte(i + 1)
		keys[i] = secp256k1.PrivKeyFromBytes(raw)
	}
	pub := func(i int) string { return hex.EncodeToString(keys[i].PubKey().SerializeCompressed()) }
	terms := Terms{Version: Version, Game: "test", Network: "simnet", Table: "table", Kind: kind, Atoms: 1000000, LockBlocks: 8, Identity: pub(0), Recovery: pub(1)}
	if kind != "seatbond" {
		terms.Members = []string{pub(2), pub(1)}
	}
	return Input{Terms: terms, Outpoint: wire.OutPoint{Index: 1}}, keys
}
func sign(t *testing.T, tx *wire.MsgTx, input Input, key *secp256k1.PrivateKey) []byte {
	t.Helper()
	hash, err := SignatureHash(tx, 0, input)
	if err != nil {
		t.Fatal(err)
	}
	return ecdsa.Sign(key, hash).Serialize()
}
func TestRefundAuthorityAndTimelock(t *testing.T) {
	for _, kind := range []string{"seatbond", "stake", "tablebond"} {
		t.Run(kind, func(t *testing.T) {
			input, keys := fixture(t, kind)
			tx, err := Refund(input, []byte{txscript.OP_TRUE}, 1000)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = RefundWitness(tx, 0, input, sign(t, tx, input, keys[0])); err == nil {
				t.Fatal("game identity spent deposit")
			}
			witness, err := RefundWitness(tx, 0, input, sign(t, tx, input, keys[1]))
			if err != nil {
				t.Fatal(err)
			}
			tx.TxIn[0].SignatureScript = witness
			if err = VerifySpend(tx, []Input{input}, chaincfg.SimNetParams()); err != nil {
				t.Fatal(err)
			}
			// Re-sign the immature transaction so rejection exercises CSV, not signature failure.
			tx.TxIn[0].Sequence--
			tx.TxIn[0].SignatureScript, err = RefundWitness(tx, 0, input, sign(t, tx, input, keys[1]))
			if err != nil {
				t.Fatal(err)
			}
			if err = VerifySpend(tx, []Input{input}, chaincfg.SimNetParams()); err == nil {
				t.Fatal("immature refund accepted")
			}
		})
	}
}
func TestCooperativePayoutRequiresEveryMember(t *testing.T) {
	input, keys := fixture(t, "stake")
	tx, err := Refund(input, []byte{txscript.OP_TRUE}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	tx.TxIn[0].Sequence = wire.MaxTxInSequenceNum
	sigs := map[string][]byte{}
	for _, key := range keys[1:] {
		sigs[hex.EncodeToString(key.PubKey().SerializeCompressed())] = sign(t, tx, input, key)
	}
	tx.TxIn[0].SignatureScript, err = SettlementWitness(tx, 0, input, sigs)
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifySpend(tx, []Input{input}, chaincfg.SimNetParams()); err != nil {
		t.Fatal(err)
	}
	tx.TxOut[0].Value--
	if err = VerifySpend(tx, []Input{input}, chaincfg.SimNetParams()); err == nil {
		t.Fatal("changed payout accepted")
	}
	tx.TxOut[0].Value++
	delete(sigs, input.Terms.Recovery)
	if _, err = SettlementWitness(tx, 0, input, sigs); err == nil {
		t.Fatal("missing loser signature accepted")
	}
}
func TestCanonicalDescriptors(t *testing.T) {
	input, _ := fixture(t, "stake")
	original, err := input.Terms.Script()
	if err != nil {
		t.Fatal(err)
	}
	input.Terms.Members[0], input.Terms.Members[1] = strings.ToUpper(input.Terms.Members[1]), strings.ToUpper(input.Terms.Members[0])
	reordered, err := input.Terms.Script()
	if err != nil || !bytes.Equal(original, reordered) {
		t.Fatalf("noncanonical keys: %v", err)
	}
	input.Terms.Members[0] = strings.ToLower(input.Terms.Members[1])
	if _, err = input.Terms.Script(); err == nil {
		t.Fatal("duplicate member accepted")
	}
	for _, lock := range []uint32{0, 65536, 1 << 31} {
		input, _ = fixture(t, "stake")
		input.Terms.LockBlocks = lock
		if _, err = input.Terms.Script(); err == nil {
			t.Fatal("invalid lock accepted")
		}
	}
	for _, kind := range []string{"forfeitbond", "bond", "unknown"} {
		input, _ = fixture(t, "seatbond")
		input.Terms.Kind = kind
		if _, err = input.Terms.Script(); err == nil {
			t.Fatal("unknown template accepted")
		}
	}
}
func TestIntentIncludesWitnessMetadata(t *testing.T) {
	input, _ := fixture(t, "stake")
	tx, err := Refund(input, []byte{txscript.OP_TRUE}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	other := tx.Copy()
	other.TxIn[0].SignatureScript = []byte{1, 2}
	if !SameIntent(tx, other) {
		t.Fatal("signature assembly changes intent")
	}
	other.TxIn[0].ValueIn++
	if tx.TxHash() != other.TxHash() || SameIntent(tx, other) {
		t.Fatal("witness value not bound")
	}
	raw, _ := tx.Bytes()
	if _, err = DecodeTransaction(append(raw, 0)); err == nil {
		t.Fatal("trailing bytes accepted")
	}
}
func TestMaximumRosterFitsConsensus(t *testing.T) {
	input, _ := fixture(t, "stake")
	input.Terms.Members = nil
	for i := 0; i < MaxMembers; i++ {
		raw := make([]byte, 32)
		raw[31] = byte(i + 2)
		key := secp256k1.PrivKeyFromBytes(raw)
		input.Terms.Members = append(input.Terms.Members, hex.EncodeToString(key.PubKey().SerializeCompressed()))
	}
	script, err := input.Terms.Script()
	if err != nil {
		t.Fatal(err)
	}
	if len(script) > txscript.MaxScriptElementSize {
		t.Fatal("oversized script")
	}
}
