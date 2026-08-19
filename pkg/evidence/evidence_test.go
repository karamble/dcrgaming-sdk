package evidence

import (
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
)

const evMatch = "bs-evidence-match"

// cellBand mirrors commitment.CellBand: attestations sit above every log
// sequence, one per cell.
const cellBand = uint64(1) << 32

// evPriv is the fixed log key behind the fixtures, so the tests are reproducible.
func evPriv() *secp256k1.PrivateKey {
	b, _ := hex.DecodeString("5a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90")
	return secp256k1.PrivKeyFromBytes(b)
}

func fill(v byte) [32]byte {
	var out [32]byte
	for i := range out {
		out[i] = v
	}
	return out
}

// cellDigest recomputes spec 7.4's cell-attestation digest from literals, an
// independent reference for the messages the store retains.
func cellDigest(x, y uint8, occupied bool, salt [32]byte) [32]byte {
	h := blake256.New()
	h.Write([]byte("battleships/cellattest/v1"))
	o := byte(0)
	if occupied {
		o = 1
	}
	h.Write([]byte{x, y, o})
	h.Write(salt[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// divergentCell builds the two halves of one cell equivocation: a play-time
// attestation over one occupancy and a reveal-side attestation over the other,
// at the same band position. Two log keys wrap the one private key with
// independent books, as a cheat spanning two sessions would, because a single
// key's own book refuses the divergent re-sign outright.
func divergentCell(t *testing.T) (pub *secp256k1.PublicKey, pos forfeit.Position,
	digA [32]byte, sigA []byte, digB [32]byte, sigB []byte) {
	t.Helper()
	priv := evPriv()
	pub = priv.PubKey()
	x, y := uint8(3), uint8(5)
	seq := cellBand + uint64(y)*10 + uint64(x)
	pos = forfeit.Position{Match: evMatch, Domain: forfeit.DomainEntry, Seq: seq}
	salt := fill(0x22)

	k1, err := forfeit.LogKeyFrom(priv, evMatch)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := forfeit.LogKeyFrom(priv, evMatch)
	if err != nil {
		t.Fatal(err)
	}
	digA = cellDigest(x, y, false, salt)
	digB = cellDigest(x, y, true, salt)
	sigA, err = k1.Sign(forfeit.DomainEntry, seq, digA[:])
	if err != nil {
		t.Fatal(err)
	}
	sigB, err = k2.Sign(forfeit.DomainEntry, seq, digB[:])
	if err != nil {
		t.Fatal(err)
	}
	return pub, pos, digA, sigA, digB, sigB
}

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evidence.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s, path
}

// TestRecordDivergenceRecoversKey is the headline: two divergent cell
// attestations at one band position hand back the cheat's key, and it is the key
// that signed them.
func TestRecordDivergenceRecoversKey(t *testing.T) {
	s, _ := openTemp(t)
	pub, pos, digA, sigA, digB, sigB := divergentCell(t)

	if k, err := s.Record(pub, pos, digA, sigA); err != nil || k != nil {
		t.Fatalf("first half: key=%v err=%v, want no key and no error", k, err)
	}
	k, err := s.Record(pub, pos, digB, sigB)
	if err != nil {
		t.Fatalf("second half: %v", err)
	}
	if k == nil {
		t.Fatal("a divergent second half returned no key")
	}
	if !k.Key().PubKey().IsEqual(pub) {
		t.Fatal("the recovered key is not the signer's")
	}
	if got := s.Retained(pub, pos); len(got) != 2 {
		t.Fatalf("store kept %d halves, want both", len(got))
	}
}

// TestIdenticalReissueNoRecovery is the complement: the same attestation seen
// twice is a no-op, kept once, and exposes nothing.
func TestIdenticalReissueNoRecovery(t *testing.T) {
	s, _ := openTemp(t)
	pub, pos, digA, sigA, _, _ := divergentCell(t)

	if k, err := s.Record(pub, pos, digA, sigA); err != nil || k != nil {
		t.Fatalf("first half: key=%v err=%v", k, err)
	}
	k, err := s.Record(pub, pos, digA, sigA)
	if err != nil {
		t.Fatalf("re-issue: %v", err)
	}
	if k != nil {
		t.Fatal("an identical re-issue returned a key")
	}
	if got := s.Retained(pub, pos); len(got) != 1 {
		t.Fatalf("store kept %d halves after a re-issue, want 1", len(got))
	}
}

// TestSurvivesRestart is the spec's restore-and-replay requirement: the first
// half is written to disk, the store is reopened, and the divergent half seen
// after the restart still completes the proof.
func TestSurvivesRestart(t *testing.T) {
	s1, path := openTemp(t)
	pub, pos, digA, sigA, digB, sigB := divergentCell(t)

	if _, err := s1.Record(pub, pos, digA, sigA); err != nil {
		t.Fatalf("first half: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	k, err := s2.Record(pub, pos, digB, sigB)
	if err != nil {
		t.Fatalf("second half after restart: %v", err)
	}
	if k == nil || !k.Key().PubKey().IsEqual(pub) {
		t.Fatal("the proof did not survive the restart")
	}

	// The retained pair recovers the same key on its own, straight from disk.
	halves := s2.Retained(pub, pos)
	if len(halves) != 2 {
		t.Fatalf("reopened store holds %d halves, want 2", len(halves))
	}
	again, err := forfeit.Recover(pub, halves[0].Digest[:], halves[0].Sig, halves[1].Digest[:], halves[1].Sig)
	if err != nil {
		t.Fatalf("recover from the reloaded halves: %v", err)
	}
	if !again.PubKey().IsEqual(pub) {
		t.Fatal("the reloaded halves recovered the wrong key")
	}
}

// TestChainHalfTriggerSurfacesRecovery exercises the M6 seam: a signature seen on
// chain is fed to the same store. It is never a Recover half itself - recovery is
// wire by wire - so it does nothing until the divergent wire pair is held, then a
// chain sighting surfaces that recovery.
func TestChainHalfTriggerSurfacesRecovery(t *testing.T) {
	s, _ := openTemp(t)
	pub, pos, digA, sigA, digB, sigB := divergentCell(t)

	if _, err := s.Record(pub, pos, digA, sigA); err != nil {
		t.Fatalf("first wire half: %v", err)
	}

	// A 65-byte on-chain signature; its bytes are opaque here.
	chainSig := make([]byte, 65)
	for i := range chainSig {
		chainSig[i] = byte(i)
	}

	// With only one wire half held, the chain half evidences but cannot recover.
	if k, err := s.Trigger(pub, pos, chainSig); err != nil || k != nil {
		t.Fatalf("chain trigger before a wire pair: key=%v err=%v, want none", k, err)
	}

	// The divergent wire half arrives; the pair recovers.
	if k, err := s.Record(pub, pos, digB, sigB); err != nil || k == nil {
		t.Fatalf("divergent wire half: key=%v err=%v", k, err)
	}

	// A later chain sighting surfaces the same recovery.
	k, err := s.Trigger(pub, pos, chainSig)
	if err != nil {
		t.Fatalf("chain trigger after the pair: %v", err)
	}
	if k == nil || !k.Key().PubKey().IsEqual(pub) {
		t.Fatal("the chain trigger did not surface the wire recovery")
	}
}
