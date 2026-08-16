package membership

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// The vectors below are assembled from literals, never from the code under
// test, so a layout change cannot regenerate them to match itself - it can
// only fail here, before it strands every table formed under the old layout.

const (
	goldenTermsHash  = "398a281024e3fb3ae4991517f6db469e8c4cddfd2b63ac20dffa78e1590e94ad"
	goldenRosterHash = "cb3ad6fe2b038e666d11fa210435f082dcd38aa6f71e9412aa7536295a2da340"
)

// goldenTerms is the fixed table the pins are derived from. Every field is an
// independent literal, so no two can swap places without a pin failing.
func goldenTerms() Terms {
	return Terms{
		Game:       "poker",
		GameVer:    5,
		SID:        "1f2e3d4c5b6a7988",
		BuyInAtoms: 25_000_000,
		Seats:      2,
		CSVBlocks:  144,
		Until:      987654,
	}
}

// goldenTermsPreimage lays the digest input out byte by byte: the tag raw,
// strings be32-length-prefixed, the game version widened to a big-endian
// int64, every other number at its declared width.
func goldenTermsPreimage() []byte {
	pre := make([]byte, 0, 78)
	pre = append(pre, "gaming/table/terms/v1"...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x05) // be32(5), the game name's length
	pre = append(pre, "poker"...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x05) // be64(5), the game version
	pre = append(pre, 0x00, 0x00, 0x00, 0x10)                         // be32(16), the session id's length
	pre = append(pre, "1f2e3d4c5b6a7988"...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x00, 0x01, 0x7d, 0x78, 0x40) // be64(25000000), the buy-in
	pre = append(pre, 0x00, 0x00, 0x00, 0x02)                         // be32(2), the seats
	pre = append(pre, 0x00, 0x00, 0x00, 0x90)                         // be32(144), the refund timelock
	pre = append(pre, 0x00, 0x0f, 0x12, 0x06)                         // be32(987654), the admission deadline
	return pre
}

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

func TestTheTermsHashLayoutIsPinned(t *testing.T) {
	pre := goldenTermsPreimage()
	if len(pre) != 78 {
		t.Fatalf("the pinned preimage is %d bytes, want 78, so this test's own assembly is wrong", len(pre))
	}
	sum := blake256.Sum256(pre)
	if got := hex.EncodeToString(sum[:]); got != goldenTermsHash {
		t.Fatalf("the assembled preimage hashes to %s, want %s", got, goldenTermsHash)
	}

	th, err := goldenTerms().Hash()
	if err != nil {
		t.Fatalf("terms hash: %v", err)
	}
	if got := hex.EncodeToString(th[:]); got != goldenTermsHash {
		t.Fatalf("Terms.Hash is %s, want the pinned %s", got, goldenTermsHash)
	}
}

// The standalone match id form: the table id alone, or with the session
// appended after a pipe. Everything downstream files its state under these.
func TestTheStandaloneMatchIdFormIsPinned(t *testing.T) {
	if got := MatchID("0abc12de34f05678", ""); got != "0abc12de34f05678" {
		t.Fatalf("a table alone derives %q, want the table id unchanged", got)
	}
	if got := MatchID("0abc12de34f05678", "77ee55dd"); got != "0abc12de34f05678|77ee55dd" {
		t.Fatalf("a table and session derive %q, want %q", got, "0abc12de34f05678|77ee55dd")
	}
}

func TestTheFormationMatchIdIsPinned(t *testing.T) {
	privA := privFromHex(t, "5a1f8c3e92b7d4066e0d21c58f3a97b1442c6d80e5f9032a7b8c1d4e6f50a293")
	privB := privFromHex(t, "7c390d5f24a8b6e1093f5c7d8e2a40b661d94f03c25a8e7b190d3f6c4b28a5e0")
	keyA := privA.PubKey().SerializeCompressed()
	keyB := privB.PubKey().SerializeCompressed()
	if bytes.Compare(keyA, keyB) >= 0 {
		t.Fatal("the literal keys no longer sort A before B, so this vector's canonical order is wrong")
	}

	// The roster preimage: tag raw, the terms digest, a be32 member count,
	// then the members in canonical order.
	pre := make([]byte, 0, 124)
	pre = append(pre, "gaming/table/roster/v1"...)
	termsSum := blake256.Sum256(goldenTermsPreimage())
	pre = append(pre, termsSum[:]...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x02) // be32(2), the member count
	pre = append(pre, keyA...)
	pre = append(pre, keyB...)
	if len(pre) != 124 {
		t.Fatalf("the pinned preimage is %d bytes, want 124, so this test's own assembly is wrong", len(pre))
	}
	sum := blake256.Sum256(pre)
	if got := hex.EncodeToString(sum[:]); got != goldenRosterHash {
		t.Fatalf("the assembled preimage hashes to %s, want %s", got, goldenRosterHash)
	}

	rh, err := RosterHash(goldenTerms(), [][]byte{keyA, keyB})
	if err != nil {
		t.Fatalf("roster hash: %v", err)
	}
	if got := hex.EncodeToString(rh[:]); got != goldenRosterHash {
		t.Fatalf("RosterHash is %s, want the pinned %s", got, goldenRosterHash)
	}

	// The formation form is that digest rendered as hex, and nothing else.
	f, err := NewFormation(goldenTerms(), testCreds(t, privA))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}
	j, err := SignJoin(goldenTerms(), testCreds(t, privB))
	if err != nil {
		t.Fatalf("sign join: %v", err)
	}
	if err := f.AddJoin(j); err != nil {
		t.Fatalf("add join: %v", err)
	}
	id, ok := f.MatchID()
	if !ok {
		t.Fatalf("a formed table has no match id (state %s)", f.State())
	}
	if id != goldenRosterHash {
		t.Fatalf("the formation match id is %s, want the pinned %s", id, goldenRosterHash)
	}
}
