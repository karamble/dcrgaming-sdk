// Package finance defines public, canonical financial contracts. It has no
// wallet, secret-key, network, or approval capability. Games may inspect these
// contracts; only the bridge can authorize spending them.
package finance

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
)

const Version = 2
const MaxLockBlocks = 65535
const MaxAtoms int64 = 21000000 * 100000000

// MaxMembers is bounded by the redeem script push limit, not a game's seats.
// Each ECDSA check uses 35 bytes. The largest allowed template is 500 bytes.
const MaxMembers = 13

type Terms struct {
	Version    uint32   `json:"version"`
	Game       string   `json:"game"`
	Network    string   `json:"network"`
	Account    uint32   `json:"account"`
	Table      string   `json:"table"`
	Kind       string   `json:"kind"`
	Atoms      int64    `json:"atoms"`
	LockBlocks uint32   `json:"lockBlocks"`
	Identity   string   `json:"identity"`
	Recovery   string   `json:"recovery"`
	Members    []string `json:"members,omitempty"`
}

func PublicKey(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 33 {
		return nil, fmt.Errorf("expected compressed public key")
	}
	p, err := secp256k1.ParsePubKey(b)
	if err != nil || !bytes.Equal(p.SerializeCompressed(), b) {
		return nil, fmt.Errorf("invalid compressed public key")
	}
	return b, nil
}

// Canonical normalizes keys before sorting or comparing descriptors. Callers
// must persist this result rather than arbitrary caller-provided hex casing.
func (t Terms) Canonical() (Terms, error) {
	if t.Version != Version || t.Game == "" || t.Network == "" || t.Table == "" {
		return Terms{}, fmt.Errorf("incomplete scope or unsupported financial version")
	}
	if t.Atoms <= 0 || t.Atoms > MaxAtoms {
		return Terms{}, fmt.Errorf("invalid deposit amount")
	}
	if t.LockBlocks == 0 || t.LockBlocks > MaxLockBlocks {
		return Terms{}, fmt.Errorf("invalid block refund delay")
	}
	identity, err := PublicKey(t.Identity)
	if err != nil {
		return Terms{}, err
	}
	recovery, err := PublicKey(t.Recovery)
	if err != nil {
		return Terms{}, err
	}
	if bytes.Equal(identity, recovery) {
		return Terms{}, fmt.Errorf("game identity cannot own funds")
	}
	t.Identity, t.Recovery = hex.EncodeToString(identity), hex.EncodeToString(recovery)
	members := make([]string, len(t.Members))
	for i, raw := range t.Members {
		key, err := PublicKey(raw)
		if err != nil {
			return Terms{}, err
		}
		members[i] = hex.EncodeToString(key)
	}
	sort.Strings(members)
	ours := false
	for i, key := range members {
		if key == t.Identity || (i > 0 && key == members[i-1]) {
			return Terms{}, fmt.Errorf("duplicate member or game identity used for spending")
		}
		ours = ours || key == t.Recovery
	}
	switch t.Kind {
	case "seatbond":
		if len(members) != 0 {
			return Terms{}, fmt.Errorf("admission bond has no cooperative branch")
		}
	case "stake", "tablebond":
		if len(members) < 2 || len(members) > MaxMembers || !ours {
			return Terms{}, fmt.Errorf("invalid financial roster")
		}
	default:
		return Terms{}, fmt.Errorf("unsupported deposit kind %q", t.Kind)
	}
	t.Members = members
	return t, nil
}

func (t Terms) Script() ([]byte, error) {
	t, err := t.Canonical()
	if err != nil {
		return nil, err
	}
	identity, _ := hex.DecodeString(t.Identity)
	recovery, _ := hex.DecodeString(t.Recovery)
	if t.Kind == "seatbond" {
		return AdmissionScript(identity, recovery, t.LockBlocks)
	}
	members := make([][]byte, len(t.Members))
	for i, key := range t.Members {
		members[i], _ = hex.DecodeString(key)
	}
	return CooperativeScript(recovery, members, t.LockBlocks)
}

func AdmissionScript(identity, recovery []byte, blocks uint32) ([]byte, error) {
	if _, err := PublicKey(hex.EncodeToString(identity)); err != nil {
		return nil, err
	}
	if _, err := PublicKey(hex.EncodeToString(recovery)); err != nil {
		return nil, err
	}
	if bytes.Equal(identity, recovery) || blocks == 0 || blocks > MaxLockBlocks {
		return nil, fmt.Errorf("invalid admission authority or refund delay")
	}
	return txscript.NewScriptBuilder().AddData(identity).AddOp(txscript.OP_DROP).AddInt64(int64(blocks)).AddOp(txscript.OP_CHECKSEQUENCEVERIFY).AddOp(txscript.OP_DROP).AddData(recovery).AddOp(txscript.OP_CHECKSIGVERIFY).AddOp(txscript.OP_TRUE).Script()
}
func CooperativeScript(owner []byte, members [][]byte, blocks uint32) ([]byte, error) {
	if blocks == 0 || blocks > MaxLockBlocks || len(members) < 2 || len(members) > MaxMembers {
		return nil, fmt.Errorf("invalid cooperative terms")
	}
	keys := make([][]byte, len(members))
	for i, key := range members {
		if _, err := PublicKey(hex.EncodeToString(key)); err != nil {
			return nil, err
		}
		keys[i] = append([]byte(nil), key...)
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i], keys[j]) < 0 })
	ours := false
	b := txscript.NewScriptBuilder().AddOp(txscript.OP_IF)
	for i, key := range keys {
		if i > 0 && bytes.Equal(key, keys[i-1]) {
			return nil, fmt.Errorf("duplicate financial member")
		}
		ours = ours || bytes.Equal(key, owner)
		b.AddData(key).AddOp(txscript.OP_CHECKSIGVERIFY)
	}
	if !ours {
		return nil, fmt.Errorf("owner is not a financial member")
	}
	b.AddOp(txscript.OP_ELSE).AddInt64(int64(blocks)).AddOp(txscript.OP_CHECKSEQUENCEVERIFY).AddOp(txscript.OP_DROP).AddData(owner).AddOp(txscript.OP_CHECKSIGVERIFY).AddOp(txscript.OP_ENDIF).AddOp(txscript.OP_TRUE)
	script, err := b.Script()
	if err != nil {
		return nil, err
	}
	if len(script) > txscript.MaxScriptElementSize {
		return nil, fmt.Errorf("financial script exceeds consensus push limit")
	}
	return script, nil
}

func (t Terms) Output(params stdaddr.AddressParams) (string, []byte, error) {
	script, err := t.Script()
	if err != nil {
		return "", nil, err
	}
	addr, err := stdaddr.NewAddressScriptHashV0(script, params)
	if err != nil {
		return "", nil, err
	}
	_, pk := addr.PaymentScript()
	return addr.String(), pk, nil
}
