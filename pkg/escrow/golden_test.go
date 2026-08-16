package escrow

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"
)

// The vectors below are assembled from literals, never from the code under
// test, so a layout change cannot regenerate them to match itself - it can
// only fail here, before a one-byte drift sends a deposit somewhere nobody
// can spend it.

const (
	goldenRedeemHex     = "632103413cd76706482cf339e15b57701b7a6843b459b41232f0d19207524bdbc8757a52bf2103548f21ec4e636b3704c56307da438ea1783c759932202c85220a1e6a3736c21252bf2103af5e04babf346972d67746c26dd2775c9dcd9605123d1ab9ed9ab2c341bf843f52bf51670140b2752103af5e04babf346972d67746c26dd2775c9dcd9605123d1ab9ed9ab2c341bf843f52bf5168"
	goldenDepositAddr   = "DcmtzgmXggCmtc5sjYcvDbWCjVSiFkddz2k"
	goldenDepositScript = "a9149e5fe1f87151c5cc9eb0a200084322d1972976a787"
	goldenPoPDigest     = "a65daca7887822383db60e9261aec21f7e1c77deeac7641c215d7844e0fe7b4c"
	goldenPoPOutpoint   = "e4d3c2b1a09f8e7d6c5b4a392817065f4e3d2c1b0a998877665544332211ffee:2"
)

// privFromHex pins a private key to a literal. A fixture drawn fresh cannot be
// pinned, and one derived from the code under test tracks its mutations.
func privFromHex(t *testing.T, s string) *secp256k1.PrivateKey {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("the literal %q is not a 32-byte scalar", s)
	}
	return secp256k1.PrivKeyFromBytes(b)
}

// goldenMembers returns the three fixed member keys and the owner among them,
// in the canonical order the script lays them out in, guarded below.
func goldenMembers(t *testing.T) (members [][]byte, owner []byte) {
	t.Helper()
	k1 := privFromHex(t, "2f5c8b1e4d7a09361c2e5b8f0a4d7c1963b2e5d8f14a7c0e3d6b9a2c5e8f1047").PubKey().SerializeCompressed()
	k2 := privFromHex(t, "4a7d0c3f6e9b2581d4f7a0c3e6b9d2f5081b4e7a0d3c6f9b2e5a8d1c4f70b3e6").PubKey().SerializeCompressed()
	k3 := privFromHex(t, "691e4b7d0a3c6f92e5b8d1f4a7c0e3961d4b7a0e3d6c9f2b5e8a1d4c7f0a3b6d").PubKey().SerializeCompressed()
	if bytes.Compare(k1, k3) >= 0 || bytes.Compare(k3, k2) >= 0 {
		t.Fatal("the literal keys no longer sort k1, k3, k2, so this vector's canonical order is wrong")
	}
	return [][]byte{k1, k3, k2}, k2
}

func TestTheRedeemScriptBytesArePinned(t *testing.T) {
	canonical, owner := goldenMembers(t)

	// The layout: OP_IF, one push-key/schnorr/OP_CHECKSIGALTVERIFY per member
	// in canonical order, OP_TRUE, OP_ELSE, the timelock, OP_CHECKSEQUENCEVERIFY,
	// OP_DROP, the owner's own check, OP_TRUE, OP_ENDIF.
	want := make([]byte, 0, 153)
	want = append(want, 0x63) // OP_IF
	for _, m := range canonical {
		want = append(want, 0x21) // a 33-byte push
		want = append(want, m...)
		want = append(want, 0x52, 0xbf) // schnorr sig type, OP_CHECKSIGALTVERIFY
	}
	want = append(want, 0x51, 0x67) // OP_TRUE, OP_ELSE
	want = append(want, 0x01, 0x40) // a one-byte push of 64, the refund timelock
	want = append(want, 0xb2, 0x75) // OP_CHECKSEQUENCEVERIFY, OP_DROP
	want = append(want, 0x21)
	want = append(want, owner...)
	want = append(want, 0x52, 0xbf, 0x51, 0x68) // schnorr, verify, OP_TRUE, OP_ENDIF
	if len(want) != 153 {
		t.Fatalf("the pinned script is %d bytes, want 153, so this test's own assembly is wrong", len(want))
	}
	if got := hex.EncodeToString(want); got != goldenRedeemHex {
		t.Fatalf("the assembled script is %s, want %s", got, goldenRedeemHex)
	}

	// Members are handed over out of canonical order on purpose; sorting them
	// is part of what the pin covers.
	redeem, err := RedeemScript(owner, [][]byte{canonical[0], owner, canonical[1]}, 64)
	if err != nil {
		t.Fatalf("redeem script: %v", err)
	}
	if got := hex.EncodeToString(redeem); got != goldenRedeemHex {
		t.Fatalf("RedeemScript is %s, want the pinned %s", got, goldenRedeemHex)
	}
}

func TestTheDepositAddressIsPinned(t *testing.T) {
	redeem, err := hex.DecodeString(goldenRedeemHex)
	if err != nil {
		t.Fatalf("the pinned script literal does not decode: %v", err)
	}
	a, pkScript, err := Address(redeem, chaincfg.MainNetParams())
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	if got := a.String(); got != goldenDepositAddr {
		t.Fatalf("the deposit address is %s, want the pinned %s", got, goldenDepositAddr)
	}
	if got := hex.EncodeToString(pkScript); got != goldenDepositScript {
		t.Fatalf("the deposit pkScript is %s, want the pinned %s", got, goldenDepositScript)
	}
}

func TestTheBondPoPDigestLayoutIsPinned(t *testing.T) {
	holder := privFromHex(t, "0d3f6a9c2e5b8d1f4a7c0e3b6d9f2a5c81e4b7d0a3f6c9e2b5d8a1c4e7f0b396").PubKey().SerializeCompressed()

	// The layout: tag raw, then the outpoint and the holder, each
	// be32-length-prefixed.
	pre := make([]byte, 0, 127)
	pre = append(pre, "dcrpoker/bond/pop/v1"...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x42) // be32(66), the outpoint's length
	pre = append(pre, goldenPoPOutpoint...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x21) // be32(33), the holder's length
	pre = append(pre, holder...)
	if len(pre) != 127 {
		t.Fatalf("the pinned preimage is %d bytes, want 127, so this test's own assembly is wrong", len(pre))
	}
	sum := blake256.Sum256(pre)
	if got := hex.EncodeToString(sum[:]); got != goldenPoPDigest {
		t.Fatalf("the assembled preimage hashes to %s, want %s", got, goldenPoPDigest)
	}

	d, err := BondPoPDigest(goldenPoPOutpoint, holder)
	if err != nil {
		t.Fatalf("pop digest: %v", err)
	}
	if got := hex.EncodeToString(d[:]); got != goldenPoPDigest {
		t.Fatalf("BondPoPDigest is %s, want the pinned %s", got, goldenPoPDigest)
	}

	// A signature over the pinned digest itself must satisfy the verifier,
	// which ties the verify path to exactly these bytes.
	ownerPriv := privFromHex(t, "37a1d4c7f00b3e6d91c4a7d0b3f6c992e5b8f1a4d7c0e3b6a9d2c5f8e1b4a707")
	bond, err := BondScript(ownerPriv.PubKey().SerializeCompressed(), MinBondBlocks)
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}
	pinned, err := hex.DecodeString(goldenPoPDigest)
	if err != nil {
		t.Fatalf("the pinned digest literal does not decode: %v", err)
	}
	sig, err := schnorr.Sign(ownerPriv, pinned)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := VerifyBondPoP(bond, goldenPoPOutpoint, holder, sig.Serialize()); err != nil {
		t.Fatalf("a proof signed over the pinned digest was refused: %v", err)
	}
}
