package gamelog

import (
	"testing"

	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
)

func TestACheckpointIsSignedAndVerifies(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}

	cp, err := c.Checkpoint(0, 7, []int64{1200, 800}, privs[0])
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := cp.Verify(); err != nil {
		t.Fatalf("an honest checkpoint did not verify: %v", err)
	}

	for _, tc := range []struct {
		name string
		bend func(*Checkpoint)
	}{
		{"different stacks", func(x *Checkpoint) { x.Stacks = []int64{2000, 0} }},
		{"a different hand", func(x *Checkpoint) { x.Hand = 8 }},
		{"a different seat", func(x *Checkpoint) { x.Seat = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := *cp
			bad.Stacks = append([]int64(nil), cp.Stacks...)
			tc.bend(&bad)
			if err := bad.Verify(); err == nil {
				t.Fatal("a tampered checkpoint verified")
			}
		})
	}
}

func TestACheckpointIsRefusedOnBadTerms(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	elsewhere, err := forfeit.NewLogKey("another-table")
	if err != nil {
		t.Fatalf("log key: %v", err)
	}

	for _, tc := range []struct {
		name   string
		seat   uint32
		stacks []int64
		key    *forfeit.LogKey
	}{
		{"a seat not at the table", 9, []int64{1000, 1000}, privs[0]},
		{"another seat's key", 1, []int64{1000, 1000}, privs[0]},
		{"a key from another match", 0, []int64{1000, 1000}, elsewhere},
		{"the wrong number of stacks", 0, []int64{1000}, privs[0]},
		{"a negative stack", 0, []int64{1000, -1}, privs[0]},
		{"no key", 0, []int64{1000, 1000}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Checkpoint(tc.seat, 1, tc.stacks, tc.key); err == nil {
				t.Fatal("signed a checkpoint that should have been refused")
			}
		})
	}
}

// The third way to equivocate, closed like the other two. A seat that told one
// player the chips landed one way and another that they landed differently
// could settle against whichever of them looked away first.
func TestSigningTwoCheckpointsForOneHandHandsOverTheKey(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}

	a, err := c.Checkpoint(0, 7, []int64{1200, 800}, privs[0])
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	b, err := c.Checkpoint(0, 7, []int64{1900, 100}, cheating(privs[0]))
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := ConflictingCheckpoints(a, b); err != nil {
		t.Fatalf("two different checkpoints at one hand were not a conflict: %v", err)
	}
	got, err := RecoverKeyFromCheckpoints(a, b)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !got.PubKey().IsEqual(privs[0].Public()) {
		t.Fatal("recovered a key that is not the one that signed both")
	}
}

// And signing the same checkpoint twice - which happens whenever a message is
// resent - must hand over nothing.
func TestResendingACheckpointLeaksNothing(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	a, _ := c.Checkpoint(0, 7, []int64{1200, 800}, privs[0])
	b, _ := c.Checkpoint(0, 7, []int64{1200, 800}, privs[0])

	if string(a.Sig) != string(b.Sig) {
		t.Fatal("signing the same checkpoint twice was not deterministic")
	}
	if err := ConflictingCheckpoints(a, b); err == nil {
		t.Fatal("resending a checkpoint was treated as equivocation")
	}
	if _, err := RecoverKeyFromCheckpoints(a, b); err == nil {
		t.Fatal("resending a checkpoint exposed the key")
	}
}

// A checkpoint at hand 5 and a log entry at sequence 5 are both routine and both
// honest. Without separate nonce domains they would share a nonce and a seat
// would publish its own key for playing correctly.
func TestACheckpointAndAnEntryAtTheSameNumberLeakNothing(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}

	e := c.Next(0, 5, StreetPreFlop, ActionCheck, 0)
	if err := e.Sign(privs[0]); err != nil {
		t.Fatalf("sign: %v", err)
	}
	cp, err := c.Checkpoint(0, 5, []int64{1000, 1000}, privs[0])
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	att, err := c.AttestHead(0, privs[0])
	if err != nil {
		t.Fatalf("attest: %v", err)
	}

	if string(e.Sig[:32]) == string(cp.Sig[:32]) {
		t.Fatal("an entry and a checkpoint at the same number shared a nonce")
	}
	if string(att.Sig[:32]) == string(cp.Sig[:32]) {
		t.Fatal("an attestation and a checkpoint at the same number shared a nonce")
	}

	eh, err := e.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	d := cp.Digest()
	if _, err := forfeit.Recover(privs[0].Public(), eh[:], e.Sig, d[:], cp.Sig); err == nil {
		t.Fatal("signing an entry and a checkpoint at one number exposed the key")
	}
}

// Heights record when a table did things. The only thing about a self-reported
// height that can be checked is that it does not go backwards - a peer that is
// genuinely behind and one lying about it are indistinguishable - so that is
// what is checked, and nothing is claimed beyond it.
func TestHeightsDoNotGoBackwards(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}

	at := func(seat uint32, height uint32) *Entry {
		t.Helper()
		e := c.Next(seat, 1, StreetPreFlop, ActionCheck, 0)
		e.Height = height
		if err := e.Sign(privs[seat]); err != nil {
			t.Fatalf("sign: %v", err)
		}
		return e
	}

	if err := c.Append(at(0, 1_100_000)); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Standing still is ordinary - several actions inside one block is the
	// normal case, not a fault.
	if err := c.Append(at(1, 1_100_000)); err != nil {
		t.Fatalf("two entries in one block were refused: %v", err)
	}
	if err := c.Append(at(0, 1_100_003)); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Going backwards is not.
	if err := c.Append(at(1, 1_100_002)); err == nil {
		t.Fatal("an entry claiming an earlier height than the one before it was accepted")
	}
}

// The height is covered by the signature, so it cannot be adjusted in flight to
// make a table look faster or slower than it was.
func TestAHeightCannotBeChangedInFlight(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	e := c.Next(0, 1, StreetPreFlop, ActionCheck, 0)
	e.Height = 1_100_000
	if err := e.Sign(privs[0]); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := e.Verify(); err != nil {
		t.Fatalf("an honest entry did not verify: %v", err)
	}
	e.Height = 1_100_500
	if err := e.Verify(); err == nil {
		t.Fatal("an entry with its height altered still verified")
	}
}

// And carrying a height must not weaken the signing itself: a seat playing a
// whole hand at rising heights still leaks nothing.
func TestHeightsDoNotLeakTheKey(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	type sig struct{ hash, sig []byte }
	var seen []sig

	for i := range 12 {
		seat := uint32(i % 2)
		e := c.Next(seat, 1, StreetPreFlop, ActionCheck, 0)
		e.Height = uint32(1_100_000 + i)
		if err := e.Sign(privs[seat]); err != nil {
			t.Fatalf("sign: %v", err)
		}
		if err := c.Append(e); err != nil {
			t.Fatalf("append: %v", err)
		}
		if seat != 0 {
			continue
		}
		h, err := e.Hash()
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		for _, s := range seen {
			if string(s.sig[:32]) == string(e.Sig[:32]) {
				t.Fatal("a hand played at rising heights reused a nonce")
			}
			if _, err := forfeit.Recover(privs[0].Public(), s.hash, s.sig, h[:], e.Sig); err == nil {
				t.Fatal("a hand played at rising heights exposed the key")
			}
		}
		seen = append(seen, sig{hash: h[:], sig: e.Sig})
	}
}
