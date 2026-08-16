package gamelog

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"

	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
)

// checkpointTag domain-separates a checkpoint from every other thing a seat
// signs, so one can never be read as another.
var checkpointTag = []byte("dcrpoker/gamelog/checkpoint/v1")

// Checkpoint is what the stacks were when a hand ended, signed by one seat.
//
// It is the last thing everybody agreed on, and that is its whole job. A hand in
// progress is not agreed - one player is mid-decision and the other has not seen
// it - so there is nothing to settle it by. A hand boundary is different: the
// chips have moved, the pot is pushed, and every seat can sign the same numbers.
//
// That makes a checkpoint the only safe thing to settle on when a table stops
// unexpectedly. Money moves to where the last mutually-signed checkpoint says it
// was, and the hand that was under way is void - which is also why nobody has to
// work out who walked off. The answer does not depend on it.
//
// Signed under its own domain and indexed by hand rather than by sequence
// number. Both matter: without the domain, a checkpoint at hand 5 and an entry
// at sequence 5 would share a nonce and a seat would publish its own key by
// playing correctly.
type Checkpoint struct {
	// Hand is the hand just completed. Hand 0 is the table's opening
	// position, before anything was played.
	Hand uint64
	// Stacks is what every seat holds, indexed by seat.
	Stacks []int64
	Seat   uint32
	Signer []byte
	Sig    []byte
}

func (c *Checkpoint) signingBytes() []byte {
	var b bytes.Buffer
	b.Write(checkpointTag)
	_ = binary.Write(&b, binary.BigEndian, c.Hand)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(c.Stacks)))
	for _, s := range c.Stacks {
		_ = binary.Write(&b, binary.BigEndian, s)
	}
	_ = binary.Write(&b, binary.BigEndian, c.Seat)
	b.Write(c.Signer)
	return b.Bytes()
}

// Digest is what a checkpoint's signature covers.
func (c *Checkpoint) Digest() [32]byte {
	return blake256.Sum256(c.signingBytes())
}

// Checkpoint signs the stacks at the end of a hand for a seat.
//
// The stacks come from the caller rather than from the chain, because this
// package records what happened and does not compute it - pkg/replay folds the
// log into the stacks, and both peers reach the same numbers independently
// before either signs. A seat that signs stacks it did not derive is trusting
// whoever handed them over.
func (ch *Chain) Checkpoint(seat uint32, hand uint64, stacks []int64, key *forfeit.LogKey) (*Checkpoint, error) {
	if key == nil {
		return nil, fmt.Errorf("no signing key")
	}
	if key.Match() != ch.matchID {
		return nil, fmt.Errorf("that log key belongs to match %s, not %s", key.Match(), ch.matchID)
	}
	if len(stacks) != len(ch.roster) {
		return nil, fmt.Errorf("a checkpoint carries %d stacks for a table of %d", len(stacks), len(ch.roster))
	}
	for i, s := range stacks {
		if s < 0 {
			return nil, fmt.Errorf("seat %d cannot hold %d chips", i, s)
		}
	}
	pub := key.Public().SerializeCompressed()
	want, ok := ch.roster[seat]
	if !ok {
		return nil, fmt.Errorf("seat %d is not at this table", seat)
	}
	if !bytes.Equal(pub, want) {
		return nil, fmt.Errorf("key does not belong to seat %d", seat)
	}

	c := &Checkpoint{
		Hand:   hand,
		Stacks: append([]int64(nil), stacks...),
		Seat:   seat,
		Signer: pub,
	}
	d := c.Digest()
	sig, err := key.Sign(forfeit.DomainCheckpoint, hand, d[:])
	if err != nil {
		return nil, fmt.Errorf("sign checkpoint: %w", err)
	}
	c.Sig = sig
	return c, nil
}

// Verify checks a checkpoint's signature against the key it names.
func (c *Checkpoint) Verify() error {
	if len(c.Sig) != SigLen {
		return fmt.Errorf("signature is %d bytes, want %d", len(c.Sig), SigLen)
	}
	pub, err := secp256k1.ParsePubKey(c.Signer)
	if err != nil {
		return fmt.Errorf("signer key: %w", err)
	}
	sig, err := schnorr.ParseSignature(c.Sig)
	if err != nil {
		return fmt.Errorf("parse signature: %w", err)
	}
	d := c.Digest()
	if !sig.Verify(d[:], pub) {
		return fmt.Errorf("signature does not verify for seat %d", c.Seat)
	}
	return nil
}

// ConflictingCheckpoints reports whether one seat signed two different sets of
// stacks for one hand.
//
// The third way to equivocate, and it had to be closed alongside the other two:
// a seat that could tell one player the chips landed one way and another that
// they landed differently would be able to settle against whichever of them
// stopped watching first.
func ConflictingCheckpoints(a, b *Checkpoint) error {
	if a == nil || b == nil {
		return fmt.Errorf("two checkpoints required")
	}
	if a.Seat != b.Seat {
		return fmt.Errorf("checkpoints are from different seats (%d and %d)", a.Seat, b.Seat)
	}
	if !bytes.Equal(a.Signer, b.Signer) {
		return fmt.Errorf("checkpoints are signed by different keys")
	}
	if a.Hand != b.Hand {
		return fmt.Errorf("checkpoints are for different hands (%d and %d)", a.Hand, b.Hand)
	}
	if a.Digest() == b.Digest() {
		return fmt.Errorf("checkpoints agree; there is nothing in dispute here")
	}
	if err := a.Verify(); err != nil {
		return fmt.Errorf("first checkpoint: %w", err)
	}
	if err := b.Verify(); err != nil {
		return fmt.Errorf("second checkpoint: %w", err)
	}
	return nil
}

// RecoverKeyFromCheckpoints turns two conflicting checkpoints into the key that
// signed them, the same way the other two routes to equivocation do.
func RecoverKeyFromCheckpoints(a, b *Checkpoint) (*secp256k1.PrivateKey, error) {
	if err := ConflictingCheckpoints(a, b); err != nil {
		return nil, err
	}
	da, db := a.Digest(), b.Digest()
	pub, err := secp256k1.ParsePubKey(a.Signer)
	if err != nil {
		return nil, fmt.Errorf("signer key: %w", err)
	}
	return forfeit.Recover(pub, da[:], a.Sig, db[:], b.Sig)
}
