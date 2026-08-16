package gamelog

import (
	"testing"

	"github.com/decred/dcrd/crypto/blake256"

	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
)

// The whole point of the log, end to end.
//
// Until now this package could prove a seat had lied and could do nothing
// about it. These check that the proof is now a key.

// A seat tells one player it folded and another that it raised.
func TestEquivocatingOnAnActionHandsOverTheKey(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}

	folded := c.Next(0, 1, StreetPreFlop, ActionFold, 0)
	if err := folded.Sign(privs[0]); err != nil {
		t.Fatalf("sign: %v", err)
	}
	raised := c.Next(0, 1, StreetPreFlop, ActionRaise, 500)
	if err := raised.Sign(cheating(privs[0])); err != nil {
		t.Fatalf("sign: %v", err)
	}

	// Each recipient sees a perfectly valid entry; neither can tell alone.
	for _, e := range []*Entry{folded, raised} {
		if err := e.Verify(); err != nil {
			t.Fatalf("an equivocating entry did not verify on its own: %v", err)
		}
	}

	p := EquivocationProof{A: *folded, B: *raised}
	if err := p.Verify(); err != nil {
		t.Fatalf("the proof did not verify: %v", err)
	}
	got, err := p.RecoverKey()
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !got.PubKey().IsEqual(privs[0].Public()) {
		t.Fatal("recovered a key that is not the one that equivocated")
	}
}

// The other route: forking history rather than lying about one action. Both
// must end at the same key, or a cheat could choose the cheaper one.
func TestForkingHistoryHandsOverTheSameKey(t *testing.T) {
	privs, roster := testSeats(t, 2)

	one, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	other, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	if err := one.Append(signed(t, one, privs, 0, ActionCheck, 0)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := other.Append(equivocated(t, other, privs, 0, ActionBet, 100)); err != nil {
		t.Fatalf("append: %v", err)
	}

	a, err := one.AttestHead(1, privs[1])
	if err != nil {
		t.Fatalf("attest: %v", err)
	}
	b, err := other.AttestHead(1, cheating(privs[1]))
	if err != nil {
		t.Fatalf("attest: %v", err)
	}

	got, err := RecoverKeyFromHeads(a, b)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !got.PubKey().IsEqual(privs[1].Public()) {
		t.Fatal("recovered a key that is not the one that forked history")
	}
}

// The direction that matters more: a seat that plays an entire hand correctly
// must never hand over anything. A seat records actions and attests to heads at
// overlapping sequence numbers all game long, and if those collided the honest
// player would forfeit their bond for playing properly.
func TestPlayingAWholeHandHonestlyLeaksNothing(t *testing.T) {
	privs, roster := testSeats(t, 2)
	c, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}

	type signature struct {
		hash []byte
		sig  []byte
	}
	var seen []signature
	note := func(hash, sig []byte) {
		for _, s := range seen {
			if string(s.sig[:32]) == string(sig[:32]) {
				t.Fatal("an honest hand reused a nonce")
			}
			pub := privs[0].Public()
			if _, err := forfeit.Recover(pub, s.hash, s.sig, hash, sig); err == nil {
				t.Fatal("an honest hand exposed a seat's key")
			}
		}
		seen = append(seen, signature{hash: hash, sig: sig})
	}

	for i := range 12 {
		seat := uint32(i % 2)
		e := c.Next(seat, 1, StreetPreFlop, ActionCheck, 0)
		if err := e.Sign(privs[seat]); err != nil {
			t.Fatalf("sign: %v", err)
		}
		if err := c.Append(e); err != nil {
			t.Fatalf("append: %v", err)
		}
		// Seat 0 also attests to the head at every sequence number, which
		// is exactly the overlap that would be fatal without domains.
		att, err := c.AttestHead(0, privs[0])
		if err != nil {
			t.Fatalf("attest: %v", err)
		}
		if seat == 0 {
			h, err := e.Hash()
			if err != nil {
				t.Fatalf("hash: %v", err)
			}
			note(h[:], e.Sig)
		}
		d := blake256.Sum256(att.signingBytes())
		note(d[:], att.Sig)
	}
}

// An attestation for a chain the key does not belong to is refused, so a log
// key cannot be used at a table it was not drawn for - where its positions
// would collide with the ones it already used.
func TestAttestingWithAnotherMatchesKeyIsRefused(t *testing.T) {
	_, roster := testSeats(t, 2)
	c, err := NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	elsewhere, err := forfeit.NewLogKey("another-table")
	if err != nil {
		t.Fatalf("log key: %v", err)
	}
	if _, err := c.AttestHead(0, elsewhere); err == nil {
		t.Fatal("attested to a head with a key from another match")
	}
}
