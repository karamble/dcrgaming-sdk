package forfeit

import (
	"bytes"
	"errors"
	"testing"

	"github.com/decred/dcrd/crypto/blake256"
)

const positionMatch = "match-for-positions"

func testKey(t *testing.T) *LogKey {
	t.Helper()
	k, err := NewLogKey(positionMatch)
	if err != nil {
		t.Fatalf("log key: %v", err)
	}
	return k
}

func digestOf(s string) [32]byte { return blake256.Sum256([]byte(s)) }

// One position, one message - and what happens when a caller gets that wrong.
//
// Sign's whole mechanism is a nonce fixed by position, so signing two different
// messages at one position hands over the key. That is the punishment, and it is
// correct for anything whose content its position determines. It is a loaded gun
// pointed at the caller for anything else, and the precondition was stated in a
// comment and enforced nowhere.
//
// The way it goes wrong is not adversarial. A table whose hand counter restarts
// signs hand 1 twice, with a different deck the second time, and forfeits its own
// bond to whoever kept the first signature. So the invariant is enforced here now,
// where the key is, rather than trusted at every call site.

func TestSigningOneMessageTwiceAtOnePositionIsFine(t *testing.T) {
	k := testKey(t)
	digest := digestOf("the same message")

	first, err := k.Sign(DomainEntry, 5, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	again, err := k.Sign(DomainEntry, 5, digest[:])
	if err != nil {
		t.Fatalf("sign again: %v", err)
	}
	if !bytes.Equal(first, again) {
		t.Fatal("the same message at one position produced two different signatures")
	}
}

// The refusal. Without it this call publishes the signing key.
func TestSigningTwoMessagesAtOnePositionIsRefused(t *testing.T) {
	k := testKey(t)
	a := digestOf("one thing")
	b := digestOf("another thing")

	if _, err := k.Sign(DomainEntry, 5, a[:]); err != nil {
		t.Fatalf("sign: %v", err)
	}
	sig, err := k.Sign(DomainEntry, 5, b[:])
	if err == nil {
		t.Fatal("a second, different message at one position was signed; that publishes the key")
	}
	if sig != nil {
		t.Fatal("a refused signature still returned bytes")
	}
}

// A different position is a different question, and a different domain is too -
// which is what lets a seat sign an entry and a head attestation at sequence 5.
func TestOtherPositionsAreUnaffected(t *testing.T) {
	k := testKey(t)
	a := digestOf("one thing")
	b := digestOf("another thing")

	if _, err := k.Sign(DomainEntry, 5, a[:]); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := k.Sign(DomainEntry, 6, b[:]); err != nil {
		t.Fatalf("the next sequence number was refused: %v", err)
	}
	if _, err := k.Sign(DomainHead, 5, b[:]); err != nil {
		t.Fatalf("another domain at the same sequence was refused: %v", err)
	}
}

// A message-committed signature carries the digest into its nonce, so signing two
// different things at one position is two ordinary signatures and no disclosure.
// That is for content a position does not determine - a freshly drawn card key, a
// shuffle with fresh blinding - where the position scheme cannot be used safely.
func TestCommittedSignaturesDoNotPublishTheKey(t *testing.T) {
	k := testKey(t)
	a := digestOf("a card key")
	b := digestOf("a different card key")

	sigA, err := k.SignCommitted(DomainCardKey, 1, a[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	sigB, err := k.SignCommitted(DomainCardKey, 1, b[:])
	if err != nil {
		t.Fatalf("a committed signature at a used position must be allowed: %v", err)
	}
	if bytes.Equal(sigA, sigB) {
		t.Fatal("two different messages produced one signature")
	}

	if _, err := Recover(k.Public(), a[:], sigA, b[:], sigB); err == nil {
		t.Fatal("two committed signatures at one position yielded the signing key")
	}
}

// Repeating a committed signature is still byte-identical, which the repair
// discipline depends on: anything that must arrive is said again, and a second
// telling has to be the same bytes rather than a second signature.
func TestRepeatingACommittedSignatureIsIdentical(t *testing.T) {
	k := testKey(t)
	digest := digestOf("a card key")

	first, err := k.SignCommitted(DomainCardKey, 1, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	again, err := k.SignCommitted(DomainCardKey, 1, digest[:])
	if err != nil {
		t.Fatalf("sign again: %v", err)
	}
	if !bytes.Equal(first, again) {
		t.Fatal("saying the same frame twice produced two different signatures")
	}
}

// A domain belongs to one scheme, and using the other one is a mistake rather
// than a choice. Otherwise a call site added later can quietly put content a
// position does not determine back onto the position nonce.
func TestADomainCannotBeSignedBothWays(t *testing.T) {
	k := testKey(t)
	digest := digestOf("x")

	if _, err := k.SignCommitted(DomainEntry, 5, digest[:]); err == nil {
		t.Fatal("a log entry was signed with a committed nonce, which disables forfeiture for it")
	}
	if _, err := k.Sign(DomainCardKey, 1, digest[:]); err == nil {
		t.Fatal("a card key was signed with a position nonce, which is what publishes the key")
	}
}

// failingBook remembers in memory and cannot reach whatever should outlive it.
type failingBook struct {
	mem  memoryBook
	fail error
}

func (b *failingBook) Used(p Position) ([32]byte, bool) { return b.mem.Used(p) }
func (b *failingBook) Record(p Position, digest [32]byte) error {
	b.mem.Record(p, digest)
	return b.fail
}

// A position the book could not record is a position that must not be signed.
// Otherwise a signature leaves the process while the only durable memory of it
// failed - a full disk would quietly disarm the book, and a restart after it
// would sign the position again over whatever the new deck says.
func TestAPositionTheBookCannotRecordIsNotSigned(t *testing.T) {
	k := testKey(t)
	book := &failingBook{fail: errors.New("disk full")}
	k.Remember(book)
	digest := digestOf("an entry")

	if _, err := k.Sign(DomainEntry, 7, digest[:]); err == nil {
		t.Fatal("signed a position the book failed to record")
	}

	// Once the book can write again, the same message signs fine: the failure
	// refused a signature, it did not burn the position.
	book.fail = nil
	if _, err := k.Sign(DomainEntry, 7, digest[:]); err != nil {
		t.Fatalf("sign after the book recovered: %v", err)
	}
	// And a different message there is still refused, exactly as if the
	// failure had never happened.
	other := digestOf("a different entry")
	if _, err := k.Sign(DomainEntry, 7, other[:]); err == nil {
		t.Fatal("a failed record weakened the one-position-one-message rule")
	}
}

// A challenge says nothing its position does not, so it signs by position; the
// secrets it obliges were drawn fresh, so they sign committed. Each refused the
// other way round.
func TestChallengeAndSecretsSignOnlyTheirOwnWay(t *testing.T) {
	k := testKey(t)
	digest := digestOf("x")

	if _, err := k.Sign(DomainChallenge, 2, digest[:]); err != nil {
		t.Fatalf("a challenge is positional and was refused: %v", err)
	}
	if _, err := k.SignCommitted(DomainChallenge, 2, digest[:]); err == nil {
		t.Fatal("a challenge signed with a committed nonce")
	}
	if _, err := k.SignCommitted(DomainSecrets, 2, digest[:]); err != nil {
		t.Fatalf("secrets are committed and were refused: %v", err)
	}
	if _, err := k.Sign(DomainSecrets, 2, digest[:]); err == nil {
		t.Fatal("secrets signed on the position nonce, which a second reveal would leak the key through")
	}
}
