// Package escrow builds the per-depositor escrow scripts used to hold a
// player's stake for a hand.
//
// The scripts are shared between the poker server and the client: both sides
// must derive byte-identical scripts from the same table, since the script hash
// is the deposit address and a one-byte disagreement sends money somewhere
// nobody can spend it. Writing the construction twice is how that happens, so
// it lives here once.
package escrow

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/karamble/dcrgaming-sdk/pkg/finance"
)

const (
	// MaxMembers caps a table at the referee's SNG/WTA seat limit. The
	// settlement branch carries one key per member, so this also bounds the
	// script size.
	MaxMembers = finance.MaxMembers

	// PubKeyLen is the length of a compressed secp256k1 public key.
	PubKeyLen = 33

	// SigLen is the length of a Decred consensus Schnorr signature: 64 bytes
	// of [r,s] plus a trailing hash type byte.
	SigLen = 65

	// MaxCSVBlocks is the largest relative timelock that can actually be
	// spent.
	//
	// A timelocked branch is satisfied by putting the lock in the spending
	// input's sequence, and consensus compares only the low 16 bits of it
	// (wire.SequenceLockTimeMask). Anything larger builds a script whose
	// branch no sequence can ever satisfy - which is not a script that
	// matures late, it is a script that never matures. Coin paid into one
	// is gone, so this is checked wherever a lock is turned into a script.
	MaxCSVBlocks = 0xffff

	// scriptVersion is the only script version these escrows use.
	scriptVersion = 0
)

// RedeemScript derives the bridge-owned ECDSA cooperative/refund script.
func RedeemScript(owner []byte, members [][]byte, csvBlocks uint32) ([]byte, error) {
	return RecoveryRedeemScript(owner, members, owner, csvBlocks)
}

// RecoveryRedeemScript keeps cooperative signing keys separate from the
// unilateral refund key. The latter may be held by the player's bridge.
func RecoveryRedeemScript(owner []byte, members [][]byte, recovery []byte, csvBlocks uint32) ([]byte, error) {
	if !bytes.Equal(owner, recovery) {
		return nil, fmt.Errorf("financial owner must hold its refund key")
	}
	return finance.CooperativeScript(owner, members, csvBlocks)
}

// CanonicalMembers returns members sorted by compressed key bytes. The order
// fixes both the script layout and the order signatures are supplied in, so
// every participant derives the same script and the same signing order from the
// same set of keys regardless of seat assignment.
func CanonicalMembers(members [][]byte) ([][]byte, error) {
	if len(members) == 0 {
		return nil, fmt.Errorf("no table members")
	}
	if len(members) > MaxMembers {
		return nil, fmt.Errorf("%d members exceeds the maximum of %d", len(members), MaxMembers)
	}

	out := make([][]byte, 0, len(members))
	for i, m := range members {
		if err := checkPubKey(m); err != nil {
			return nil, fmt.Errorf("member %d: %w", i, err)
		}
		out = append(out, append([]byte(nil), m...))
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i], out[j]) < 0 })
	for i := 1; i < len(out); i++ {
		if bytes.Equal(out[i-1], out[i]) {
			return nil, fmt.Errorf("duplicate table member key")
		}
	}
	return out, nil
}

// MemberCount reports how many signatures a redeem script's settlement branch
// requires, by counting the checks before the refund branch begins.
func MemberCount(redeem []byte) (int, error) {
	tokenizer := txscript.MakeScriptTokenizer(scriptVersion, redeem)
	n := 0
	for tokenizer.Next() {
		switch tokenizer.Opcode() {
		case txscript.OP_CHECKSIGVERIFY:
			n++
		case txscript.OP_ELSE:
			if err := tokenizer.Err(); err != nil {
				return 0, err
			}
			if n == 0 {
				return 0, fmt.Errorf("settlement branch has no signature checks")
			}
			return n, nil
		}
	}
	if err := tokenizer.Err(); err != nil {
		return 0, fmt.Errorf("parse redeem script: %w", err)
	}
	return 0, fmt.Errorf("redeem script has no refund branch")
}

// Members returns the settlement branch's member keys in the order the script
// checks them, which is the order SettlementSigScript expects signatures in.
//
// Reading the roster back out of the script means a spend is assembled against
// the script it actually has to satisfy, rather than against a list recorded
// somewhere else that may since have drifted from it.
func Members(redeem []byte) ([][]byte, error) {
	tokenizer := txscript.MakeScriptTokenizer(scriptVersion, redeem)
	var (
		keys    [][]byte
		pending []byte
	)
	for tokenizer.Next() {
		switch tokenizer.Opcode() {
		case txscript.OP_DATA_33:
			pending = tokenizer.Data()
		case txscript.OP_CHECKSIGVERIFY:
			if len(pending) != PubKeyLen {
				return nil, fmt.Errorf("signature check %d is not preceded by a key", len(keys))
			}
			keys = append(keys, append([]byte(nil), pending...))
			pending = nil
		case txscript.OP_ELSE:
			if len(keys) == 0 {
				return nil, fmt.Errorf("settlement branch has no signature checks")
			}
			return keys, nil
		}
	}
	if err := tokenizer.Err(); err != nil {
		return nil, fmt.Errorf("parse redeem script: %w", err)
	}
	return nil, fmt.Errorf("redeem script has no refund branch")
}

// Address returns the P2SH address a redeem script's deposits are paid to,
// along with the pkScript that funds it.
func Address(redeem []byte, params stdaddr.AddressParams) (stdaddr.Address, []byte, error) {
	a, err := stdaddr.NewAddressScriptHash(scriptVersion, redeem, params)
	if err != nil {
		return nil, nil, err
	}
	_, pkScript := a.PaymentScript()
	return a, pkScript, nil
}

func checkPubKey(key []byte) error {
	if len(key) != PubKeyLen {
		return fmt.Errorf("got %d bytes, want a %d byte compressed key", len(key), PubKeyLen)
	}
	if _, err := secp256k1.ParsePubKey(key); err != nil {
		return fmt.Errorf("not a valid public key: %w", err)
	}
	return nil
}

func containsKey(keys [][]byte, want []byte) bool {
	for _, k := range keys {
		if bytes.Equal(k, want) {
			return true
		}
	}
	return false
}
