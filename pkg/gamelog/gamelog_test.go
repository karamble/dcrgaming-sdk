package gamelog

import (
	"bytes"
	"go/build"
	"strings"
	"testing"

	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
)

const testMatch = "table1|sess1"

func testSeats(t *testing.T, n int) ([]*forfeit.LogKey, Roster) {
	t.Helper()
	privs := make([]*forfeit.LogKey, n)
	roster := make(Roster, n)
	for i := range privs {
		priv, err := forfeit.NewLogKey(testMatch)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		privs[i] = priv
		roster[uint32(i)] = priv.Public().SerializeCompressed()
	}
	return privs, roster
}

// signed builds and signs an entry at the chain's head for a seat.
func signed(t *testing.T, c *Chain, privs []*forfeit.LogKey, seat uint32, action Action, amount int64) *Entry {
	t.Helper()
	e := c.Next(seat, 1, StreetPreFlop, action, amount)
	if err := e.Sign(privs[seat]); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return e
}

// equivocated signs like signed, from a key that has forgotten what it already
// put its name to. Only a cheat or a restarted process is in that position.
func equivocated(t *testing.T, c *Chain, privs []*forfeit.LogKey, seat uint32, action Action, amount int64) *Entry {
	t.Helper()
	e := c.Next(seat, 1, StreetPreFlop, action, amount)
	if err := e.Sign(cheating(privs[seat])); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return e
}

// The log is the account of the game and must not depend on any transport that
// happens to carry it, generated types least of all. A test enforces it, because
// an import added in passing would be invisible otherwise.
func TestNoTransportDependency(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("inspect package: %v", err)
	}
	for _, imp := range pkg.Imports {
		if strings.Contains(imp, "grpc") || strings.Contains(imp, "protobuf") {
			t.Errorf("gamelog imports %q; it must stay independent of the transport", imp)
		}
	}
}

func TestChainAcceptsAValidSequence(t *testing.T) {
	privs, roster := testSeats(t, 3)
	c, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}

	head, seq := c.Head()
	if head != GenesisHash(testMatch) || seq != 0 {
		t.Fatalf("a new chain should start at the match genesis")
	}

	for i, seat := range []uint32{0, 1, 2} {
		e := signed(t, c, privs, seat, ActionCheck, 0)
		if err := c.Append(e); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if c.Len() != 3 {
		t.Fatalf("chain holds %d entries, want 3", c.Len())
	}
	if _, seq := c.Head(); seq != 3 {
		t.Fatalf("head is at seq %d, want 3", seq)
	}
}

// Each of these is a way the history could be rewritten if it were not checked.
func TestChainRejectsBrokenEntries(t *testing.T) {
	privs, roster := testSeats(t, 3)
	stranger, err := forfeit.NewLogKey(testMatch)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	cases := []struct {
		name   string
		tamper func(t *testing.T, c *Chain, e *Entry)
		want   string
	}{{
		name:   "a seat not at the table",
		tamper: func(_ *testing.T, _ *Chain, e *Entry) { e.Seat = 9 },
		want:   "not at this table",
	}, {
		name: "someone else signing for a seat",
		tamper: func(t *testing.T, _ *Chain, e *Entry) {
			if err := e.Sign(stranger); err != nil {
				t.Fatalf("sign: %v", err)
			}
		},
		want: "signed by another key",
	}, {
		name:   "a gap in the sequence",
		tamper: func(_ *testing.T, _ *Chain, e *Entry) { e.Seq = 7 },
		want:   "expected 1",
	}, {
		name:   "chaining to the wrong history",
		tamper: func(_ *testing.T, _ *Chain, e *Entry) { e.PrevHash = [32]byte{0xff} },
		want:   "does not chain",
	}, {
		name:   "an amount altered after signing",
		tamper: func(_ *testing.T, _ *Chain, e *Entry) { e.Amount += 1 },
		want:   "does not verify",
	}, {
		name:   "an action altered after signing",
		tamper: func(_ *testing.T, _ *Chain, e *Entry) { e.Action = ActionRaise },
		want:   "does not verify",
	}, {
		name:   "a street altered after signing",
		tamper: func(_ *testing.T, _ *Chain, e *Entry) { e.Street = StreetRiver },
		want:   "does not verify",
	}, {
		name:   "money attached to a fold",
		tamper: func(_ *testing.T, _ *Chain, e *Entry) { e.Action = ActionFold; e.Amount = 100 },
		want:   "carries an amount",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewChain(testMatch, roster)
			if err != nil {
				t.Fatalf("new chain: %v", err)
			}
			e := signed(t, c, privs, 0, ActionBet, 500)
			tc.tamper(t, c, e)

			err = c.Append(e)
			if err == nil {
				t.Fatalf("expected rejection: %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %q, want something containing %q", err, tc.want)
			}
		})
	}
}

// Replaying an entry the chain already holds must fail, or a bet could be
// counted twice.
func TestChainRejectsReplay(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, _ := NewChain(testMatch, roster)

	e := signed(t, c, privs, 0, ActionBet, 500)
	if err := c.Append(e); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := c.Append(e); err == nil {
		t.Fatalf("expected a replayed entry to be rejected")
	}
}

// An entry is bound to its match, so one signed at another table cannot be
// moved into this one.
func TestEntryCannotBeReplayedAcrossMatches(t *testing.T) {
	privs, roster := testSeats(t, 2)
	first, _ := NewChain("table1|sessA", roster)
	second, _ := NewChain("table1|sessB", roster)

	e := signed(t, first, privs, 0, ActionBet, 500)
	if err := first.Append(e); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := second.Append(e); err == nil {
		t.Fatalf("an entry from another match must not chain here")
	}
}

// The proof that makes the whole log worth signing: one seat, one sequence
// number, two different signed claims.
func TestEquivocationProof(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, _ := NewChain(testMatch, roster)

	folded := signed(t, c, privs, 0, ActionFold, 0)
	raised := equivocated(t, c, privs, 0, ActionRaise, 500)

	if err := (EquivocationProof{A: *folded, B: *raised}).Verify(); err != nil {
		t.Fatalf("a seat signing two different actions at one seq is a proof: %v", err)
	}
}

func TestEquivocationProofRejectsNearMisses(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, _ := NewChain(testMatch, roster)

	seat0 := signed(t, c, privs, 0, ActionFold, 0)
	seat1 := signed(t, c, privs, 1, ActionFold, 0)

	if err := c.Append(seat0); err != nil {
		t.Fatalf("append: %v", err)
	}
	later := signed(t, c, privs, 0, ActionCheck, 0)

	cases := map[string]EquivocationProof{
		"different seats":      {A: *seat0, B: *seat1},
		"different sequences":  {A: *seat0, B: *later},
		"the same entry twice": {A: *seat0, B: *seat0},
	}
	for name, p := range cases {
		if err := p.Verify(); err == nil {
			t.Errorf("%s must not count as equivocation", name)
		}
	}
}

// Under group-chat fan-out nobody sees anyone else's stream, so a fork is found
// by comparing what each seat says the history is.
func TestConflictingHeadAttestations(t *testing.T) {
	privs, roster := testSeats(t, 2)

	one, _ := NewChain(testMatch, roster)
	if err := one.Append(signed(t, one, privs, 0, ActionBet, 100)); err != nil {
		t.Fatalf("append: %v", err)
	}
	other, _ := NewChain(testMatch, roster)
	if err := other.Append(equivocated(t, other, privs, 0, ActionBet, 900)); err != nil {
		t.Fatalf("append: %v", err)
	}

	attA, err := one.AttestHead(0, privs[0])
	if err != nil {
		t.Fatalf("attest: %v", err)
	}
	attB, err := other.AttestHead(0, privs[0])
	if err != nil {
		t.Fatalf("attest: %v", err)
	}

	if err := attA.Verify(); err != nil {
		t.Fatalf("attestation should verify: %v", err)
	}
	if err := ConflictingHeads(attA, attB); err != nil {
		t.Fatalf("two heads at one seq over different histories is a fork: %v", err)
	}

	// Agreement is not a fork, and neither is a seat attesting a later head.
	if err := ConflictingHeads(attA, attA); err == nil {
		t.Fatalf("identical attestations must not read as a fork")
	}
	if err := one.Append(signed(t, one, privs, 1, ActionCall, 100)); err != nil {
		t.Fatalf("append: %v", err)
	}
	attLater, err := one.AttestHead(0, privs[0])
	if err != nil {
		t.Fatalf("attest: %v", err)
	}
	if err := ConflictingHeads(attA, attLater); err == nil {
		t.Fatalf("attestations at different sequences must not read as a fork")
	}
}

func TestAttestHeadRefusesAKeyThatIsNotTheSeats(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, _ := NewChain(testMatch, roster)
	if _, err := c.AttestHead(0, privs[1]); err == nil {
		t.Fatalf("a seat must not be attested for with someone else's key")
	}
	if _, err := c.AttestHead(9, privs[0]); err == nil {
		t.Fatalf("a seat not at the table must not be attested for")
	}
}

// A transcript is the artifact handed to someone who trusts nobody at the
// table, so it has to verify on its own and fail if touched.
func TestTranscriptRoundTripsAndVerifies(t *testing.T) {
	privs, roster := testSeats(t, 3)
	c, _ := NewChain(testMatch, roster)

	for _, step := range []struct {
		seat   uint32
		action Action
		amount int64
	}{
		{0, ActionBet, 500}, {1, ActionCall, 500}, {2, ActionFold, 0}, {0, ActionCheck, 0},
	} {
		e := signed(t, c, privs, step.seat, step.action, step.amount)
		if err := c.Append(e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	blob, err := c.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := Unmarshal(blob)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Len() != c.Len() || back.MatchID() != c.MatchID() {
		t.Fatalf("transcript did not round trip")
	}
	wantHead, wantSeq := c.Head()
	gotHead, gotSeq := back.Head()
	if gotHead != wantHead || gotSeq != wantSeq {
		t.Fatalf("rebuilt chain has a different head")
	}
}

func TestTranscriptRejectsTampering(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, _ := NewChain(testMatch, roster)
	if err := c.Append(signed(t, c, privs, 0, ActionBet, 500)); err != nil {
		t.Fatalf("append: %v", err)
	}

	blob, err := c.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Raise the bet in the transcript without touching the signature.
	tampered := bytes.Replace(blob, []byte(`"amount": 500`), []byte(`"amount": 900`), 1)
	if bytes.Equal(tampered, blob) {
		t.Fatalf("test did not actually alter the transcript")
	}
	if _, err := Unmarshal(tampered); err == nil {
		t.Fatalf("an altered transcript must not verify")
	}
}

func TestNewChainRejectsBadRosters(t *testing.T) {
	_, roster := testSeats(t, 2)
	if _, err := NewChain("", roster); err == nil {
		t.Fatalf("a chain must name its match")
	}
	if _, err := NewChain(testMatch, Roster{0: roster[0]}); err == nil {
		t.Fatalf("a table has at least two seats")
	}
	if _, err := NewChain(testMatch, Roster{0: roster[0], 1: []byte{0x02}}); err == nil {
		t.Fatalf("a malformed key must be refused")
	}
}

// cheating makes a key forget what it has signed.
//
// A key refuses to sign one position twice, which is the guard against doing it by
// accident. Demonstrating the punishment means getting past that deliberately, the
// way a cheat running its own build or a restarted process does.
func cheating(k *forfeit.LogKey) *forfeit.LogKey {
	k.Remember(noMemory{})
	return k
}

type noMemory struct{}

func (noMemory) Used(forfeit.Position) ([32]byte, bool)  { return [32]byte{}, false }
func (noMemory) Record(forfeit.Position, [32]byte) error { return nil }
