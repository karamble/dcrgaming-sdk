package gamelog

import (
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// The vectors below are assembled from literals, never from the code under
// test, so a layout change cannot regenerate them to match itself - it can
// only fail here, before it forks every chain built under the old layout.

const (
	goldenGenesis    = "09ef832ea31414476c0b5505ae3103672e9c2aa925c2482b5110463fcfb466b6"
	goldenEntryHash  = "aadf3263547f294a6f2d835540e6eb7af6295b8265b7fa2086ba0e4f4fa7c610"
	goldenHeadDigest = "854f5ea8727db9ede71256c45c15e649864e4e485b746fe889a34c4306d0c9fd"
	goldenCheckpoint = "72f69079afbc3bff15d1a1fba36fe35932ffeadd9a84bbd0fd21a8553c299a04"
)

// goldenSigner pins a signer key to a literal scalar. A key drawn fresh cannot
// be pinned, and one derived from the code under test tracks its mutations.
func goldenSigner(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("the literal %q is not a 32-byte scalar", s)
	}
	return secp256k1.PrivKeyFromBytes(b).PubKey().SerializeCompressed()
}

func goldenBytes(t *testing.T, s string, n int) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != n {
		t.Fatalf("the literal %q is not %d bytes", s, n)
	}
	return b
}

func TestTheGenesisHashLayoutIsPinned(t *testing.T) {
	pre := make([]byte, 0, 41)
	pre = append(pre, "dcrpoker/gamelog/match/v1"...)
	pre = append(pre, "d1e2f3a4b5c60718"...)
	sum := blake256.Sum256(pre)
	if got := hex.EncodeToString(sum[:]); got != goldenGenesis {
		t.Fatalf("the assembled preimage hashes to %s, want %s", got, goldenGenesis)
	}
	g := GenesisHash("d1e2f3a4b5c60718")
	if got := hex.EncodeToString(g[:]); got != goldenGenesis {
		t.Fatalf("GenesisHash is %s, want the pinned %s", got, goldenGenesis)
	}
}

func TestTheEntryHashLayoutIsPinned(t *testing.T) {
	signer := goldenSigner(t, "3e7b1a904d5c2f68b09e8d17c4a6530f2d1b8e94a7c05f36d28419b6e0c7f5a2")
	prev := goldenBytes(t, "8899aabbccddeeff00112233445566778899aabbccddeeff0011223344556677", 32)

	// The layout: tag raw, be16 version, the previous hash, be64 sequence and
	// hand, the street byte, be32 seat, the signer, the action length-prefixed
	// with one byte, be64 amount, be32 height.
	pre := make([]byte, 0, 131)
	pre = append(pre, "dcrpoker/gamelog/entry/v1"...)
	pre = append(pre, 0x00, 0x02) // be16(2), the entry format version
	pre = append(pre, prev...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x09) // be64(9), the sequence
	pre = append(pre, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03) // be64(3), the hand
	pre = append(pre, 0x02)                                           // the turn
	pre = append(pre, 0x00, 0x00, 0x00, 0x01)                         // be32(1), the seat
	pre = append(pre, signer...)
	pre = append(pre, 0x05) // the action label's length
	pre = append(pre, "raise"...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x68) // be64(4200), the amount
	pre = append(pre, 0x00, 0x0f, 0x0f, 0x79)                         // be32(987001), the height
	if len(pre) != 131 {
		t.Fatalf("the pinned preimage is %d bytes, want 131, so this test's own assembly is wrong", len(pre))
	}
	sum := blake256.Sum256(pre)
	if got := hex.EncodeToString(sum[:]); got != goldenEntryHash {
		t.Fatalf("the assembled preimage hashes to %s, want %s", got, goldenEntryHash)
	}

	e := Entry{
		Version: 2,
		Seq:     9,
		Hand:    3,
		Street:  StreetTurn,
		Seat:    1,
		Signer:  signer,
		Action:  ActionRaise,
		Amount:  4200,
		Height:  987001,
	}
	copy(e.PrevHash[:], prev)
	h, err := e.Hash()
	if err != nil {
		t.Fatalf("entry hash: %v", err)
	}
	if got := hex.EncodeToString(h[:]); got != goldenEntryHash {
		t.Fatalf("Entry.Hash is %s, want the pinned %s", got, goldenEntryHash)
	}
}

func TestTheHeadDigestLayoutIsPinned(t *testing.T) {
	signer := goldenSigner(t, "6c2d8f0a1b3e5749d2c4a6889f1e0b357a9d4c2e6f8b013579bdf02468ace135")
	hash := goldenBytes(t, "00112233445566778899aabbccddeeff102132435465768798a9bacbdcedfe0f", 32)

	pre := make([]byte, 0, 101)
	pre = append(pre, "dcrpoker/gamelog/head/v1"...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x09) // be64(9), the sequence
	pre = append(pre, hash...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x01) // be32(1), the seat
	pre = append(pre, signer...)
	if len(pre) != 101 {
		t.Fatalf("the pinned preimage is %d bytes, want 101, so this test's own assembly is wrong", len(pre))
	}
	sum := blake256.Sum256(pre)
	if got := hex.EncodeToString(sum[:]); got != goldenHeadDigest {
		t.Fatalf("the assembled preimage hashes to %s, want %s", got, goldenHeadDigest)
	}

	att := &HeadAttestation{Seq: 9, Seat: 1, Signer: signer}
	copy(att.Hash[:], hash)
	digest := blake256.Sum256(att.signingBytes())
	if got := hex.EncodeToString(digest[:]); got != goldenHeadDigest {
		t.Fatalf("the head digest is %s, want the pinned %s", got, goldenHeadDigest)
	}
}

func TestTheCheckpointDigestLayoutIsPinned(t *testing.T) {
	signer := goldenSigner(t, "1b4e7a2d5c8f03691e2b5d8a4c7f0e3961a4d7c2b5e8f10a3d6c9b2e5f8a0d47")

	// The layout: tag raw, be64 hand, a be32 stack count, each stack as a
	// be64, be32 seat, the signer.
	pre := make([]byte, 0, 103)
	pre = append(pre, "dcrpoker/gamelog/checkpoint/v1"...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07) // be64(7), the hand
	pre = append(pre, 0x00, 0x00, 0x00, 0x03)                         // be32(3), the stack count
	pre = append(pre, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x04, 0xb0) // be64(1200)
	pre = append(pre, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0x20) // be64(800)
	pre = append(pre, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00) // be64(0)
	pre = append(pre, 0x00, 0x00, 0x00, 0x02)                         // be32(2), the seat
	pre = append(pre, signer...)
	if len(pre) != 103 {
		t.Fatalf("the pinned preimage is %d bytes, want 103, so this test's own assembly is wrong", len(pre))
	}
	sum := blake256.Sum256(pre)
	if got := hex.EncodeToString(sum[:]); got != goldenCheckpoint {
		t.Fatalf("the assembled preimage hashes to %s, want %s", got, goldenCheckpoint)
	}

	cp := &Checkpoint{Hand: 7, Stacks: []int64{1200, 800, 0}, Seat: 2, Signer: signer}
	d := cp.Digest()
	if got := hex.EncodeToString(d[:]); got != goldenCheckpoint {
		t.Fatalf("Checkpoint.Digest is %s, want the pinned %s", got, goldenCheckpoint)
	}
}
