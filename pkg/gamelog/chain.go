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

// Roster maps a seat to the session key that seat signs with. It is the same
// roster the escrow scripts commit to, and it must come from there.
//
// Nothing softer will do. Where players talk over a Bison Relay group chat, the
// group's own membership list is administered by one member, is never attested,
// can differ between members at the same generation, and can be changed just by
// receiving a message. The escrow roster is agreed on-chain by everyone whose
// money is at stake, which is the only list worth verifying signatures against.
type Roster map[uint32][]byte

// Chain is the ordered, signed history of one match.
//
// Append enforces the structure - signature, signer, sequence, linkage - and
// nothing else. Whether a seat was entitled to act is a question about the
// rules of poker, and answering it here would mean pulling the game engine into
// a package that exists to be independent of it. The caller holds both and
// checks both.
type Chain struct {
	matchID string
	roster  Roster
	entries []Entry
	head    [32]byte
	seq     uint64
}

// NewChain starts an empty chain for a match.
func NewChain(matchID string, roster Roster) (*Chain, error) {
	if matchID == "" {
		return nil, fmt.Errorf("match id required")
	}
	if len(roster) < 2 {
		return nil, fmt.Errorf("roster has %d seats, a table has at least 2", len(roster))
	}
	dup := make(Roster, len(roster))
	for seat, key := range roster {
		if len(key) != PubKeyLen {
			return nil, fmt.Errorf("seat %d key is %d bytes, want %d", seat, len(key), PubKeyLen)
		}
		if _, err := secp256k1.ParsePubKey(key); err != nil {
			return nil, fmt.Errorf("seat %d key: %w", seat, err)
		}
		dup[seat] = append([]byte(nil), key...)
	}
	return &Chain{
		matchID: matchID,
		roster:  dup,
		head:    GenesisHash(matchID),
	}, nil
}

// Head is the hash the next entry must chain to, and the sequence number it
// must carry minus one.
func (c *Chain) Head() ([32]byte, uint64) { return c.head, c.seq }

// MatchID reports which match this chain records.
func (c *Chain) MatchID() string { return c.matchID }

// Len reports how many entries the chain holds.
func (c *Chain) Len() int { return len(c.entries) }

// Signer reports the key a seat signs with, and whether the seat is at this
// table at all.
//
// This is the roster the escrow committed to, which is what makes it worth
// asking: anything checked against it is checked against the agreement that
// holds the money, not against who a message appeared to come from. Callers
// outside the log need it because the log is not the only thing a seat signs -
// the dealing is signed too, and it is verified against exactly this.
func (c *Chain) Signer(seat uint32) ([]byte, bool) {
	key, ok := c.roster[seat]
	if !ok {
		return nil, false
	}
	out := make([]byte, len(key))
	copy(out, key)
	return out, true
}

// Entries returns a copy of the chain's entries.
func (c *Chain) Entries() []Entry {
	out := make([]Entry, len(c.entries))
	copy(out, c.entries)
	return out
}

// Next prepares an unsigned entry positioned at the head of the chain, so a
// caller cannot get the linkage wrong by hand.
func (c *Chain) Next(seat uint32, hand uint64, street Street, action Action, amount int64) *Entry {
	return &Entry{
		Version:  Version,
		PrevHash: c.head,
		Seq:      c.seq + 1,
		Hand:     hand,
		Street:   street,
		Seat:     seat,
		Action:   action,
		Amount:   amount,
	}
}

// Append verifies an entry against the chain and adds it.
func (c *Chain) Append(e *Entry) error {
	if e == nil {
		return fmt.Errorf("no entry")
	}
	want, ok := c.roster[e.Seat]
	if !ok {
		return fmt.Errorf("seat %d is not at this table", e.Seat)
	}
	if !bytes.Equal(e.Signer, want) {
		return fmt.Errorf("entry for seat %d is signed by another key", e.Seat)
	}
	if e.Seq != c.seq+1 {
		return fmt.Errorf("entry is seq %d, expected %d", e.Seq, c.seq+1)
	}
	if e.PrevHash != c.head {
		return fmt.Errorf("entry does not chain to the current head")
	}
	// Heights may stand still - several actions inside one block is the
	// normal case - but they may not go backwards, which is the only thing
	// about a self-reported height that can be checked at all.
	if n := len(c.entries); n > 0 && e.Height < c.entries[n-1].Height {
		return fmt.Errorf("entry is at height %d, behind the %d the entry before it recorded",
			e.Height, c.entries[n-1].Height)
	}
	if err := e.Verify(); err != nil {
		return err
	}

	h, err := e.Hash()
	if err != nil {
		return err
	}
	c.entries = append(c.entries, *e)
	c.head = h
	c.seq = e.Seq
	return nil
}

// EquivocationProof is two entries the same seat signed at the same position.
//
// It is the whole point of signing the log. A seat that tells one player it
// folded and another that it raised has produced two signatures over the same
// sequence number, and either recipient can put them side by side. No quorum
// has to agree, no vote is taken, and nobody has to have been watching: the
// proof carries its own evidence and verifies offline, anywhere, forever.
//
// That distinction is what keeps this safe to act on. A timeout is one player's
// belief about another and can be manufactured by any group against anyone;
// this cannot be manufactured at all without the accused seat's own key.
type EquivocationProof struct {
	A, B Entry
}

// Verify checks that the proof really does show one seat signing two different
// entries at the same sequence number.
func (p EquivocationProof) Verify() error {
	if p.A.Seat != p.B.Seat {
		return fmt.Errorf("entries are from different seats (%d and %d)", p.A.Seat, p.B.Seat)
	}
	if !bytes.Equal(p.A.Signer, p.B.Signer) {
		return fmt.Errorf("entries are signed by different keys")
	}
	if p.A.Seq != p.B.Seq {
		return fmt.Errorf("entries are at different sequence numbers (%d and %d)", p.A.Seq, p.B.Seq)
	}
	ha, err := p.A.Hash()
	if err != nil {
		return fmt.Errorf("first entry: %w", err)
	}
	hb, err := p.B.Hash()
	if err != nil {
		return fmt.Errorf("second entry: %w", err)
	}
	if ha == hb {
		return fmt.Errorf("entries are identical; showing the same message twice proves nothing")
	}
	if err := p.A.Verify(); err != nil {
		return fmt.Errorf("first entry: %w", err)
	}
	if err := p.B.Verify(); err != nil {
		return fmt.Errorf("second entry: %w", err)
	}
	return nil
}

// RecoverKey turns the proof into the cheat's log key.
//
// This is what the proof was always missing. Until now an EquivocationProof
// established, beyond argument and without a quorum, that a seat had lied - and
// then nothing happened, because the escrow's branches are not conditioned on
// fraud and Decred script cannot read a proof. A player could equivocate, stop
// signing, and lose the transaction fees.
//
// The key that comes out here opens one branch of that seat's bond, for each
// player they lied to and for nobody else. See pkg/forfeit and
// escrow.ForfeitableBondScript.
func (p EquivocationProof) RecoverKey() (*secp256k1.PrivateKey, error) {
	if err := p.Verify(); err != nil {
		return nil, err
	}
	ha, err := p.A.Hash()
	if err != nil {
		return nil, err
	}
	hb, err := p.B.Hash()
	if err != nil {
		return nil, err
	}
	pub, err := secp256k1.ParsePubKey(p.A.Signer)
	if err != nil {
		return nil, fmt.Errorf("signer key: %w", err)
	}
	return forfeit.Recover(pub, ha[:], p.A.Sig, hb[:], p.B.Sig)
}

// HeadAttestation is a seat's signature over what it believes the chain head to
// be at some sequence number.
//
// Under group-chat fan-out no player sees another player's stream, so nobody
// can notice a fork just by listening. Players exchange attestations instead,
// and two of them from one seat at one sequence number over different hashes is
// the same self-contained proof an EquivocationProof is - reached from the
// other direction, by disagreement about history rather than about one action.
type HeadAttestation struct {
	Seq    uint64
	Hash   [32]byte
	Seat   uint32
	Signer []byte
	Sig    []byte
}

func (h *HeadAttestation) signingBytes() []byte {
	var b bytes.Buffer
	b.Write(headTag)
	_ = binary.Write(&b, binary.BigEndian, h.Seq)
	b.Write(h.Hash[:])
	_ = binary.Write(&b, binary.BigEndian, h.Seat)
	b.Write(h.Signer)
	return b.Bytes()
}

// AttestHead signs the chain's current head for a seat.
//
// Signed under its own domain, so that attesting to a head at sequence 5 and
// recording an entry at sequence 5 - both perfectly honest, both routine - do
// not share a nonce and hand over the seat's key for playing correctly. Two
// *different* heads at one sequence number still do, which is the point.
func (c *Chain) AttestHead(seat uint32, key *forfeit.LogKey) (*HeadAttestation, error) {
	if key == nil {
		return nil, fmt.Errorf("no signing key")
	}
	if key.Match() != c.matchID {
		return nil, fmt.Errorf("that log key belongs to match %s, not %s", key.Match(), c.matchID)
	}
	pub := key.Public().SerializeCompressed()
	want, ok := c.roster[seat]
	if !ok {
		return nil, fmt.Errorf("seat %d is not at this table", seat)
	}
	if !bytes.Equal(pub, want) {
		return nil, fmt.Errorf("key does not belong to seat %d", seat)
	}

	att := &HeadAttestation{Seq: c.seq, Hash: c.head, Seat: seat, Signer: pub}
	digest := blake256.Sum256(att.signingBytes())
	sig, err := key.Sign(forfeit.DomainHead, att.Seq, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign head: %w", err)
	}
	att.Sig = sig
	return att, nil
}

// Verify checks an attestation's signature.
func (h *HeadAttestation) Verify() error {
	if len(h.Signer) != PubKeyLen {
		return fmt.Errorf("signer key is %d bytes, want %d", len(h.Signer), PubKeyLen)
	}
	if len(h.Sig) != SigLen {
		return fmt.Errorf("signature is %d bytes, want %d", len(h.Sig), SigLen)
	}
	pub, err := secp256k1.ParsePubKey(h.Signer)
	if err != nil {
		return fmt.Errorf("signer key: %w", err)
	}
	sig, err := schnorr.ParseSignature(h.Sig)
	if err != nil {
		return fmt.Errorf("parse signature: %w", err)
	}
	digest := blake256.Sum256(h.signingBytes())
	if !sig.Verify(digest[:], pub) {
		return fmt.Errorf("attestation does not verify for seat %d", h.Seat)
	}
	return nil
}

// ConflictingHeads reports whether two attestations prove a fork: one seat
// claiming two different histories at the same point.
func ConflictingHeads(a, b *HeadAttestation) error {
	if a == nil || b == nil {
		return fmt.Errorf("two attestations required")
	}
	if a.Seat != b.Seat {
		return fmt.Errorf("attestations are from different seats (%d and %d)", a.Seat, b.Seat)
	}
	if !bytes.Equal(a.Signer, b.Signer) {
		return fmt.Errorf("attestations are signed by different keys")
	}
	if a.Seq != b.Seq {
		return fmt.Errorf("attestations are at different sequence numbers (%d and %d)", a.Seq, b.Seq)
	}
	if a.Hash == b.Hash {
		return fmt.Errorf("attestations agree; there is no fork here")
	}
	if err := a.Verify(); err != nil {
		return fmt.Errorf("first attestation: %w", err)
	}
	if err := b.Verify(); err != nil {
		return fmt.Errorf("second attestation: %w", err)
	}
	return nil
}

// RecoverKeyFromHeads turns a fork into the same key a conflicting entry would.
//
// Both routes to equivocation end in the same place, which is the point of
// giving them one domain each rather than one between them: a seat that forks
// history is punished exactly as a seat that lies about an action.
func RecoverKeyFromHeads(a, b *HeadAttestation) (*secp256k1.PrivateKey, error) {
	if err := ConflictingHeads(a, b); err != nil {
		return nil, err
	}
	da := blake256.Sum256(a.signingBytes())
	db := blake256.Sum256(b.signingBytes())
	pub, err := secp256k1.ParsePubKey(a.Signer)
	if err != nil {
		return nil, fmt.Errorf("signer key: %w", err)
	}
	return forfeit.Recover(pub, da[:], a.Sig, db[:], b.Sig)
}
