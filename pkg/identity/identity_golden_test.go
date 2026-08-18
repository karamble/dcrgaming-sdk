package identity

// The fixtures below are independent literals, never values the code under test
// produced for itself, so a derivation change cannot regenerate them to match -
// it can only fail here, before a restarted daemon derives a key that is in no
// bond on chain. The pinned pubkeys were computed once from an offline HMAC and
// pasted.

import (
	"crypto/hmac"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

const (
	// goldenSeed is the 32-byte seed the pins derive from.
	goldenSeed = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"

	// goldenSID is one fixed per-table session id.
	goldenSID = "0123456789abcdef0123456789abcdef"

	// The two domain tags the pins are taken under, pinned as literals here so a
	// tag typo in a caller is caught by a failing derivation, not regenerated.
	goldenBondTag    = "battleships/bond/v1"
	goldenSessionTag = "battleships/table-session/v1"

	// goldenBondPub pins DeriveKey(goldenBondTag, goldenSID).
	goldenBondPub = "0235feebd49e9af07a249fa17a106f97c1357d7dd4251d93b592b714e06502abe6"

	// goldenSessionPub pins DeriveKey(goldenSessionTag, goldenSID); a different
	// tag over the same seed and sid must give a different key.
	goldenSessionPub = "02b2909948cd02654e010817642a2aa1283efc64c284946bed0951f6093363d6f6"
)

func idFromSeed(t *testing.T, hexSeed string) *Identity {
	t.Helper()
	seed, err := hex.DecodeString(hexSeed)
	if err != nil || len(seed) != 32 {
		t.Fatalf("the literal %q is not a 32-byte seed", hexSeed)
	}
	return &Identity{seed: seed}
}

// The derived pubkeys are pinned, and the two tags must not collide: a client
// that derived one from the other would sign for a seat with the bond key.
func TestDerivedPubkeyGoldenIsPinned(t *testing.T) {
	id := idFromSeed(t, goldenSeed)

	bond, err := id.DeriveKey(goldenBondTag, goldenSID)
	if err != nil {
		t.Fatalf("derive bond: %v", err)
	}
	if got := hex.EncodeToString(bond.PubKey().SerializeCompressed()); got != goldenBondPub {
		t.Fatalf("bond key derives %s, want the pinned %s", got, goldenBondPub)
	}

	sess, err := id.DeriveKey(goldenSessionTag, goldenSID)
	if err != nil {
		t.Fatalf("derive session: %v", err)
	}
	if got := hex.EncodeToString(sess.PubKey().SerializeCompressed()); got != goldenSessionPub {
		t.Fatalf("session key derives %s, want the pinned %s", got, goldenSessionPub)
	}

	if goldenBondPub == goldenSessionPub {
		t.Fatal("the bond and session tags derive one key, so domain separation is gone")
	}
}

// The preimage is assembled byte by byte from literals - tag then sid, with no
// length framing between them - and run through the same HMAC by hand, so the
// layout and the value are both pinned. One byte moved and a restarted client
// derives a key in no bond on chain.
func TestDeriveKeyPreimageLayoutIsPinned(t *testing.T) {
	seed, err := hex.DecodeString(goldenSeed)
	if err != nil {
		t.Fatal(err)
	}

	pre := make([]byte, 0, len(goldenBondTag)+len(goldenSID))
	pre = append(pre, goldenBondTag...)
	pre = append(pre, goldenSID...)
	if len(pre) != len(goldenBondTag)+len(goldenSID) {
		t.Fatalf("the assembled preimage is %d bytes, so this test's own assembly is wrong", len(pre))
	}

	mac := hmac.New(blake256.New, seed)
	mac.Write(pre)
	byHand := secp256k1.PrivKeyFromBytes(mac.Sum(nil))
	if got := hex.EncodeToString(byHand.PubKey().SerializeCompressed()); got != goldenBondPub {
		t.Fatalf("the assembled preimage derives %s, want the pinned %s", got, goldenBondPub)
	}

	id := idFromSeed(t, goldenSeed)
	fromCode, err := id.DeriveKey(goldenBondTag, goldenSID)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if a, b := byHand.Serialize(), fromCode.Serialize(); string(a) != string(b) {
		t.Fatalf("DeriveKey scalar %x does not match the by-hand preimage %x", b, a)
	}

	// The order is tag-then-sid: reversing it must not reproduce the key.
	rev := hmac.New(blake256.New, seed)
	rev.Write([]byte(goldenSID))
	rev.Write([]byte(goldenBondTag))
	reversed := secp256k1.PrivKeyFromBytes(rev.Sum(nil))
	if hex.EncodeToString(reversed.PubKey().SerializeCompressed()) == goldenBondPub {
		t.Fatal("sid-then-tag derived the same key, so the input order is not pinned")
	}
}

// Load creates the seed once, reads it back the same, and refuses to generate a
// fresh one beside game state - a lost-seed signal, not a silent new player.
func TestLoadRoundTripAndGuard(t *testing.T) {
	dir := t.TempDir()
	first, err := Load(dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}

	path := filepath.Join(dir, "identity.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("identity.json was not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("identity.json is mode %o, want 600", perm)
	}

	second, err := Load(dir)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	seed1, _ := first.Backup()
	seed2, _ := second.Backup()
	if seed1 != seed2 {
		// Mutation: a second Load generating instead of reading strands the first.
		t.Fatalf("the seed changed across loads: %s then %s", seed1, seed2)
	}

	// A directory holding game state but no identity is a lost seed.
	guarded := t.TempDir()
	if err := os.WriteFile(filepath.Join(guarded, "seatbonds.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(guarded)
	if err == nil {
		// Mutation: dropping the guard generates a fresh seed over funded state.
		t.Fatal("Load generated a seed beside seatbonds.json")
	}
	if !contains(err.Error(), "seatbonds.json") {
		t.Fatalf("the guard error does not name the state that tripped it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(guarded, "identity.json")); !os.IsNotExist(err) {
		t.Fatal("the guard wrote an identity.json it should have refused to")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
