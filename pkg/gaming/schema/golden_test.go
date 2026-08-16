package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// The vectors below are assembled from literals, never from the code under
// test, so an envelope or field change cannot regenerate them to match itself
// - it can only fail here, before two peers stop reading each other's frames.

// The envelope, pinned to the exact bytes it puts on the wire and their
// sha256, for one commit built entirely from literals.
func TestTheEncodedEnvelopeIsPinned(t *testing.T) {
	const (
		wantJSON = `{"v":5,"kind":"commit","match":"b7c8d9e0f1a20314","body":{"roster":"5f00d1e2c3b4a5968778695a4b3c2d1e0f1e2d3c4b5a69788796a5b4c3d2e1f0","signer":"02b1c2d3e4f5061728394a5b6c7d8e9fa0b1c2d3e4f5061728394a5b6c7d8e9fa0","sig":"3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b"}}`
		wantSHA  = "3b1c20d027f883111898ec9010e1e598c04178ce70dde0a1378175bfd680b14b"
	)
	sum := sha256.Sum256([]byte(wantJSON))
	if got := hex.EncodeToString(sum[:]); got != wantSHA {
		t.Fatalf("the pinned envelope hashes to %s, want %s, so this test's own literals disagree", got, wantSHA)
	}

	blob, err := Encode(Version, KindCommit, "b7c8d9e0f1a20314", Commit{
		Roster: "5f00d1e2c3b4a5968778695a4b3c2d1e0f1e2d3c4b5a69788796a5b4c3d2e1f0",
		Signer: "02b1c2d3e4f5061728394a5b6c7d8e9fa0b1c2d3e4f5061728394a5b6c7d8e9fa0",
		Sig:    "3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(blob) != wantJSON {
		t.Fatalf("Encode produced %s, want the pinned %s", blob, wantJSON)
	}
	sum = sha256.Sum256(blob)
	if got := hex.EncodeToString(sum[:]); got != wantSHA {
		t.Fatalf("the encoded envelope hashes to %s, want the pinned %s", got, wantSHA)
	}
}

// The claim's wire shape, pinned as exact JSON with every field populated. The
// duty rides inside it, so this is also the pin on the duty vocabulary's shape.
func TestTheClaimJsonShapeIsPinned(t *testing.T) {
	const wantJSON = `{"seat":2,"duty":{"seat":2,"kind":"share","hand":6,"at":11},"bondOutpoint":"f1e2d3c4b5a697880f1e2d3c4b5a69788796a5b4c3d2e1f08796a5b4c3d2e1f0:0","bondScript":"76a914","tx":"010203","signer":"03d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3","sig":"9a8b7c6d5e4f30211203f4e5d6c7b8a99a8b7c6d5e4f30211203f4e5d6c7b8a99a8b7c6d5e4f30211203f4e5d6c7b8a99a8b7c6d5e4f30211203f4e5d6c7b8a9"}`

	claim := Claim{
		Seat:         2,
		Duty:         Duty{Seat: 2, Kind: DutyShare, Hand: 6, At: 11},
		BondOutpoint: "f1e2d3c4b5a697880f1e2d3c4b5a69788796a5b4c3d2e1f08796a5b4c3d2e1f0:0",
		BondScript:   "76a914",
		Tx:           "010203",
		Signer:       "03d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3",
		Sig:          "9a8b7c6d5e4f30211203f4e5d6c7b8a99a8b7c6d5e4f30211203f4e5d6c7b8a99a8b7c6d5e4f30211203f4e5d6c7b8a99a8b7c6d5e4f30211203f4e5d6c7b8a9",
	}
	if err := claim.Validate(); err != nil {
		t.Fatalf("the pinned claim does not validate: %v", err)
	}
	got, err := json.Marshal(claim)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != wantJSON {
		t.Fatalf("the claim marshals to %s, want the pinned %s", got, wantJSON)
	}

	var back Claim
	if err := json.Unmarshal([]byte(wantJSON), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != claim {
		t.Fatalf("the pinned json reads back as %+v, want %+v", back, claim)
	}
}
