package schema

import (
	"encoding/hex"
	"testing"

	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
	"github.com/karamble/dcrgaming-sdk/pkg/gamelog"
)

const cpMatch = "9bbccbcc99e2421852775868835efd6926eab532fb3286f1051f79f7572bb9b9"

func cpChain(t *testing.T, n int) (*gamelog.Chain, []*forfeit.LogKey) {
	t.Helper()
	keys := make([]*forfeit.LogKey, n)
	roster := make(gamelog.Roster, n)
	for i := range keys {
		k, err := forfeit.NewLogKey(cpMatch)
		if err != nil {
			t.Fatalf("log key: %v", err)
		}
		keys[i] = k
		roster[uint32(i)] = k.Public().SerializeCompressed()
	}
	c, err := gamelog.NewChain(cpMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	return c, keys
}

// A checkpoint has to survive the trip and still verify, or a table that stops
// has nothing to settle on.
func TestACheckpointSurvivesTheWireAndStillVerifies(t *testing.T) {
	c, keys := cpChain(t, 2)
	cp, err := c.Checkpoint(0, 7, []int64{1200, 800}, keys[0])
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	blob, err := Encode(5, KindCheckpoint, cpMatch, CheckpointFrom(cp))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	msg, err := Decode(5, blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Kind != KindCheckpoint {
		t.Fatalf("decoded kind %q, want %q", msg.Kind, KindCheckpoint)
	}
	var wire Checkpoint
	if err := msg.Into(&wire); err != nil {
		t.Fatalf("into: %v", err)
	}
	got, err := wire.Into()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if err := got.Verify(); err != nil {
		t.Fatalf("a checkpoint stopped verifying after a round trip: %v", err)
	}
	if got.Hand != cp.Hand || got.Seat != cp.Seat || len(got.Stacks) != len(cp.Stacks) {
		t.Fatal("a checkpoint changed shape crossing the wire")
	}
	for i := range got.Stacks {
		if got.Stacks[i] != cp.Stacks[i] {
			t.Fatalf("stack %d became %d, want %d", i, got.Stacks[i], cp.Stacks[i])
		}
	}
}

// And tampering with one on the way has to be caught, not absorbed.
func TestATamperedCheckpointIsRejected(t *testing.T) {
	c, keys := cpChain(t, 2)
	cp, err := c.Checkpoint(0, 7, []int64{1200, 800}, keys[0])
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	for _, tc := range []struct {
		name string
		bend func(*Checkpoint)
	}{
		{"different stacks", func(w *Checkpoint) { w.Stacks = []int64{2000, 0} }},
		{"a different hand", func(w *Checkpoint) { w.Hand = 8 }},
		{"a different seat", func(w *Checkpoint) { w.Seat = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := CheckpointFrom(cp)
			tc.bend(&w)
			got, err := w.Into()
			if err != nil {
				return // refused on the way in, which is fine
			}
			if err := got.Verify(); err == nil {
				t.Fatal("a tampered checkpoint verified")
			}
		})
	}
}

// A claim is shape-checked and nothing more, because there is nothing here to
// judge - what settles it happens on chain.
func TestAClaimIsCheckedForShapeOnly(t *testing.T) {
	good := Claim{
		Seat:         1,
		Duty:         Duty{Seat: 1, Kind: DutyAction, Hand: 3, At: 9},
		BondOutpoint: "beef:0",
		BondScript:   hex.EncodeToString([]byte{0x51}),
		Tx:           hex.EncodeToString([]byte{0x01, 0x02}),
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("a well formed claim was refused: %v", err)
	}

	for _, tc := range []struct {
		name string
		bend func(*Claim)
	}{
		{"no bond deposit", func(c *Claim) { c.BondOutpoint = "" }},
		{"no bond script", func(c *Claim) { c.BondScript = "" }},
		{"no transaction", func(c *Claim) { c.Tx = "" }},
		{"a signature with no signer", func(c *Claim) { c.Sig = "aa" }},
		{"a signer with no signature", func(c *Claim) { c.Signer = "aa" }},
		// A claim that names no obligation is the one that could be opened
		// against anybody, which is what the whole design turns on.
		{"no obligation", func(c *Claim) { c.Duty = Duty{} }},
		{"an obligation of another seat", func(c *Claim) { c.Duty.Seat = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := good
			tc.bend(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("a malformed claim was accepted")
			}
		})
	}

	// It crosses the wire like anything else.
	blob, err := Encode(5, KindClaim, cpMatch, good)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	msg, err := Decode(5, blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var back Claim
	if err := msg.Into(&back); err != nil {
		t.Fatalf("into: %v", err)
	}
	if back.Seat != good.Seat || back.BondOutpoint != good.BondOutpoint || back.Tx != good.Tx {
		t.Fatal("a claim changed crossing the wire")
	}
}
