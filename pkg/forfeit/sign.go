// Package forfeit makes cheating cost money.
//
// pkg/gamelog can already prove that a seat equivocated - told one player it
// folded and another that it raised - and the proof verifies offline, forever,
// without a quorum. Nothing can act on it. The escrow has an n-of-n settlement
// branch and a unilateral refund branch, and neither is conditioned on fraud,
// so a player who equivocates and then stops signing forces everybody to the
// CSV refund and loses the fees. Provable and unpunishable.
//
// It cannot be fixed by putting the proof in a script. Decred script verifies
// signatures over the spending transaction and nothing else; it cannot parse a
// log, compare two entries, or evaluate a claim. Any design that needs a script
// to *judge* something is dead on arrival, and any design that needs a quorum
// of players to judge it instead has handed n-1 colluding players the power to
// convict the one honest one.
//
// So the cheat is made to publish the key itself.
//
// A Schnorr signature is s = k - e*d, where k is a per-signature nonce. Derive
// k from the signing key and the *position in the log* - and deliberately not
// from the message - and one signature per position reveals nothing, while two
// signatures at one position share k and give
//
//	s1 - s2 = (e2 - e1)*d
//
// which anybody can solve for d. Equivocation is exactly signing twice at one
// position. The cheat does not get reported, judged or voted on: it hands its
// key to everyone who saw both messages.
//
// The bond's punishment branch then pays to a key only a wronged player can
// assemble, and the script does nothing cleverer than check a signature. See
// escrow.ForfeitableBondScript.
//
// # What this key must not be
//
// The leaked key is a per-match key that signs log entries and nothing else. It
// is emphatically *not* the session key: that one is named in the escrow script
// and holds the player's stake, so leaking it would let any passing observer
// take the stake rather than letting the wronged player take the bond. The
// penalty has to be bounded and it has to be directed. A log key is announced
// in the join, which is signed by the session key over a digest that covers it,
// so the roster knows whose it is and no separate binding can go missing.
// Forfeiting it costs exactly one bond.
package forfeit

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"
)

// SigLen is the length of the Schnorr signatures here: 64 bytes, no sighash
// byte, matching pkg/gamelog rather than the consensus-script signatures in
// pkg/escrow which carry one.
const SigLen = 64

// Domain says what kind of thing is being signed.
//
// This is load-bearing and easy to leave out. A seat legitimately signs a log
// entry at sequence 5 *and* a head attestation at sequence 5; those are two
// different messages, and if they shared a nonce the seat would leak its own
// key by behaving perfectly. The domain separates them, so reuse means
// equivocation and never anything else.
//
// Adding a new kind of signed message means adding a domain for it. Signing
// anything under an existing domain at a position that domain already used is
// the one thing that must never happen by accident.
type Domain string

const (
	// DomainEntry is a log entry: one per sequence number.
	DomainEntry Domain = "entry"
	// DomainHead is a head attestation: one per sequence number.
	DomainHead Domain = "head"
	// DomainCheckpoint is the agreed stacks at a hand boundary: one per
	// hand. Indexed by hand rather than by sequence number, which is safe
	// precisely because the domain keeps the two numbering schemes from
	// colliding.
	DomainCheckpoint Domain = "checkpoint"

	// The three below are the dealing, and they are all indexed by hand for
	// the same reason the checkpoint is: a seat does each of them exactly
	// once in a hand, so one hand is one position and a second signature at
	// it is equivocation rather than anything a straight player could do.
	//
	// DomainCardKey is a seat's deck key for a hand: one per hand.
	DomainCardKey Domain = "cardkey"
	// DomainShuffle is a seat's shuffle of the deck: one per hand, because
	// the round a seat shuffles in is its own seat number.
	DomainShuffle Domain = "shuffle"
	// DomainLeaving is a seat saying it is getting up: one per hand.
	DomainLeaving Domain = "leaving"

	// DomainChallenge is a seat demanding a hand be recomputed from
	// everything everybody knew: one per hand, and its content is fixed by
	// the position alone - there is nothing else a challenge says.
	DomainChallenge Domain = "challenge"
	// DomainSecrets is a seat giving a challenged hand's secrets away. The
	// secrets were drawn fresh, so the position does not fix the bytes and
	// the nonce must commit to them.
	DomainSecrets Domain = "secrets"

	// DomainShuffleComplaint is a seat disputing a shuffle it refused: one
	// per hand, its content fixed by the position - for an honest refuser
	// the claimed input deck is exactly the deck it verified against, which
	// its position determines. Position-signed on purpose: a complainer
	// that shifts its story signs two different complaints at one position,
	// and equivocation publishes its key. The deterrent is free.
	//
	// There is no answer domain. The complaint carries the refused frame
	// whole, so every peer reaches the verdict from the complaint alone -
	// nothing is owed back, and a domain for a message nobody ever needs
	// to send would be dead enforcement.
	DomainShuffleComplaint Domain = "shufflecomplaint"
)

// Which nonce a domain takes, as a property of the domain and not a choice at the
// call site. A position nonce is only safe where the position fixes the content;
// where it does not - a freshly drawn card key, a shuffle with fresh blinding - the
// same position can carry different bytes on a second attempt, and signing both
// publishes the key.
var committedDomains = map[Domain]bool{
	DomainCardKey: true,
	DomainShuffle: true,
	DomainSecrets: true,
}

var knownDomains = map[Domain]bool{
	DomainEntry:            true,
	DomainHead:             true,
	DomainCheckpoint:       true,
	DomainCardKey:          true,
	DomainShuffle:          true,
	DomainLeaving:          true,
	DomainChallenge:        true,
	DomainSecrets:          true,
	DomainShuffleComplaint: true,
}

// Committed reports whether this domain commits to the message in its nonce.
func (d Domain) Committed() bool { return committedDomains[d] }

func (d Domain) known() bool { return knownDomains[d] }

// Position is where a signature sits. Exactly one signature may ever be made at
// one position with one key.
type Position struct {
	Match  string
	Domain Domain
	Seq    uint64
}

var nonceTag = []byte("dcrpoker/forfeit/nonce/v1")

func writeField(h io.Writer, b []byte) {
	_ = binary.Write(h, binary.BigEndian, uint32(len(b)))
	_, _ = h.Write(b)
}

// nonce derives k from the key and the position, and from nothing else.
//
// Key-dependent because it must stay secret - anyone who knows k recovers d
// from a single signature, since d = (k - s)/e. Position-dependent and
// message-independent because that is the entire mechanism.
func nonce(priv *secp256k1.PrivateKey, p Position) (*secp256k1.ModNScalar, error) {
	if p.Domain == "" {
		return nil, fmt.Errorf("a signature with no domain")
	}
	if p.Match == "" {
		return nil, fmt.Errorf("a signature with no match")
	}
	key := priv.Serialize()
	defer func() {
		for i := range key {
			key[i] = 0
		}
	}()

	// The counter only ever advances past a value that is zero or over the
	// curve order, which happens with probability around 2^-128. It is not
	// message-dependent and so does not weaken anything.
	for i := uint32(0); i < 8; i++ {
		h := blake256.New()
		h.Write(nonceTag)
		writeField(h, key)
		writeField(h, []byte(p.Match))
		writeField(h, []byte(p.Domain))
		_ = binary.Write(h, binary.BigEndian, p.Seq)
		_ = binary.Write(h, binary.BigEndian, i)

		var b [32]byte
		copy(b[:], h.Sum(nil))
		var k secp256k1.ModNScalar
		overflow := k.SetBytes(&b)
		if overflow == 0 && !k.IsZero() {
			return &k, nil
		}
	}
	return nil, fmt.Errorf("could not derive a nonce for %s/%d", p.Domain, p.Seq)
}

var committedTag = []byte("dcrpoker/forfeit/nonce-committed/v1")

// committedNonce derives k from the key, the position and the message, so two
// different messages at one position take two different nonces and the pair yields
// nothing. The RFC6979 shape, and it costs exactly the property Sign provides.
func committedNonce(priv *secp256k1.PrivateKey, p Position, hash []byte) (*secp256k1.ModNScalar, error) {
	if p.Domain == "" {
		return nil, fmt.Errorf("a signature with no domain")
	}
	if p.Match == "" {
		return nil, fmt.Errorf("a signature with no match")
	}
	key := priv.Serialize()
	defer func() {
		for i := range key {
			key[i] = 0
		}
	}()

	for i := uint32(0); i < 8; i++ {
		h := blake256.New()
		h.Write(committedTag)
		writeField(h, key)
		writeField(h, []byte(p.Match))
		writeField(h, []byte(p.Domain))
		_ = binary.Write(h, binary.BigEndian, p.Seq)
		writeField(h, hash)
		_ = binary.Write(h, binary.BigEndian, i)

		var b [32]byte
		copy(b[:], h.Sum(nil))
		var k secp256k1.ModNScalar
		overflow := k.SetBytes(&b)
		if overflow == 0 && !k.IsZero() {
			return &k, nil
		}
	}
	return nil, fmt.Errorf("could not derive a nonce for %s/%d", p.Domain, p.Seq)
}

// Sign produces an EC-Schnorr-DCRv0 signature over hash, using a nonce fixed by
// position.
//
// The result is an ordinary Decred Schnorr signature and verifies with the
// stock verifier; nothing downstream needs to know it was made this way. The
// only difference is which nonce went in, and that difference is the whole
// mechanism.
func Sign(priv *secp256k1.PrivateKey, p Position, hash []byte) ([]byte, error) {
	if p.Domain.Committed() {
		return nil, fmt.Errorf("%s commits to its message and must not be signed by position; "+
			"signing it this way is what publishes the key", p.Domain)
	}
	return signWith(priv, p, hash, nonce)
}

// SignCommitted signs content a position does not determine. Equivocation on such
// a message stays provable, two conflicting signed frames being self-contained
// evidence, but is no longer self-punishing.
func SignCommitted(priv *secp256k1.PrivateKey, p Position, hash []byte) ([]byte, error) {
	if !p.Domain.Committed() {
		return nil, fmt.Errorf("%s is fixed by its position and must be signed by position, "+
			"so that equivocating on it forfeits", p.Domain)
	}
	return signWith(priv, p, hash, func(pk *secp256k1.PrivateKey, pos Position) (*secp256k1.ModNScalar, error) {
		return committedNonce(pk, pos, hash)
	})
}

func signWith(priv *secp256k1.PrivateKey, p Position, hash []byte,
	derive func(*secp256k1.PrivateKey, Position) (*secp256k1.ModNScalar, error)) ([]byte, error) {

	if priv == nil {
		return nil, fmt.Errorf("no signing key")
	}
	if len(hash) != 32 {
		return nil, fmt.Errorf("message digest is %d bytes, want 32", len(hash))
	}
	if priv.Key.IsZero() {
		return nil, fmt.Errorf("signing key is zero")
	}
	if !p.Domain.known() {
		return nil, fmt.Errorf("unknown signing domain %q", p.Domain)
	}
	k, err := derive(priv, p)
	if err != nil {
		return nil, err
	}
	defer k.Zero()

	// R = kG, negating k if R.y is odd, exactly as dcrd's schnorrSign does.
	// This is reimplemented rather than called because the library only
	// exposes signing with its own RFC6979 nonce, which depends on the
	// message and would defeat the point.
	var R secp256k1.JacobianPoint
	secp256k1.ScalarBaseMultNonConst(k, &R)
	R.ToAffine()
	if R.Y.IsOdd() {
		k.Negate()
	}
	r := &R.X

	// e = BLAKE-256(r || m)
	var input [64]byte
	r.PutBytesUnchecked(input[:32])
	copy(input[32:], hash)
	commitment := blake256.Sum256(input[:])

	var e secp256k1.ModNScalar
	if overflow := e.SetBytes(&commitment); overflow != 0 {
		// dcrd retries with a fresh nonce here. That is not available:
		// the nonce is fixed by position, and retrying would make it
		// depend on the message again and silently disable forfeiture
		// for this signature. At roughly 2^-128 this is an error rather
		// than a case to handle.
		return nil, fmt.Errorf("the commitment for %s/%d exceeded the curve order, "+
			"which is not supposed to be reachable", p.Domain, p.Seq)
	}

	// s = k - e*d
	s := new(secp256k1.ModNScalar).Mul2(&e, &priv.Key).Negate().Add(k)
	return schnorr.NewSignature(r, s).Serialize(), nil
}

// challenge recomputes e = BLAKE-256(r || m) for a signature.
func challenge(sig, hash []byte) (*secp256k1.ModNScalar, error) {
	var input [64]byte
	copy(input[:32], sig[:32])
	copy(input[32:], hash)
	commitment := blake256.Sum256(input[:])

	var e secp256k1.ModNScalar
	if overflow := e.SetBytes(&commitment); overflow != 0 {
		return nil, fmt.Errorf("commitment exceeds the curve order")
	}
	return &e, nil
}

func scalarOf(b []byte) (*secp256k1.ModNScalar, error) {
	var raw [32]byte
	copy(raw[:], b)
	var s secp256k1.ModNScalar
	if overflow := s.SetBytes(&raw); overflow != 0 {
		return nil, fmt.Errorf("signature scalar exceeds the curve order")
	}
	return &s, nil
}

// Recover extracts the signing key from two signatures made at one position.
//
// This is the punishment. It needs no cooperation, no quorum and no permission:
// anyone holding both halves of an equivocation proof can run it, and what
// comes out is the key that made both signatures.
//
// It is checked against the public key before being returned, so a caller
// cannot be handed a plausible-looking scalar derived from two signatures that
// merely happened to collide.
func Recover(pub *secp256k1.PublicKey, hashA, sigA, hashB, sigB []byte) (*secp256k1.PrivateKey, error) {
	if pub == nil {
		return nil, fmt.Errorf("no key to recover")
	}
	for _, s := range [][]byte{sigA, sigB} {
		if len(s) != SigLen {
			return nil, fmt.Errorf("signature is %d bytes, want %d", len(s), SigLen)
		}
	}
	if len(hashA) != 32 || len(hashB) != 32 {
		return nil, fmt.Errorf("message digests must be 32 bytes")
	}

	// Both signatures must carry the same r, which is what a shared nonce
	// produces and is how equivocation is spotted in the first place.
	if string(sigA[:32]) != string(sigB[:32]) {
		return nil, fmt.Errorf("these signatures do not share a nonce, so no key is exposed")
	}

	eA, err := challenge(sigA, hashA)
	if err != nil {
		return nil, err
	}
	eB, err := challenge(sigB, hashB)
	if err != nil {
		return nil, err
	}
	if eA.Equals(eB) {
		return nil, fmt.Errorf("both signatures are over the same message; " +
			"showing one signature twice proves nothing")
	}

	sA, err := scalarOf(sigA[32:])
	if err != nil {
		return nil, err
	}
	sB, err := scalarOf(sigB[32:])
	if err != nil {
		return nil, err
	}

	// s1 - s2 = (e2 - e1)*d, so d = (s1 - s2) / (e2 - e1).
	num := new(secp256k1.ModNScalar).Set(sB).Negate().Add(sA)
	den := new(secp256k1.ModNScalar).Set(eA).Negate().Add(eB)
	if den.IsZero() {
		return nil, fmt.Errorf("the two challenges are equal, so nothing can be solved")
	}
	d := new(secp256k1.ModNScalar).Mul2(num, den.InverseNonConst())
	if d.IsZero() {
		return nil, fmt.Errorf("recovered a zero key, which is not a key")
	}

	priv := secp256k1.NewPrivateKey(d)
	if !priv.PubKey().IsEqual(pub) {
		return nil, fmt.Errorf("the key recovered from these signatures is not the one that made them")
	}
	return priv, nil
}
