// Package gamelog is the signed, hash-chained record of what happened at a
// poker table.
//
// Today one server runs the game and keeps the only account of it, so a hand's
// history is exactly as trustworthy as the server: it can be rewritten, and a
// fold nobody made can be invented, with nothing a third party could check.
// This package makes the history stand on its own. Each action is signed by the
// seat that took it and chained to the one before, so a transcript can be
// handed to anyone and verified offline.
//
// It matters more once players talk to each other directly. Bison Relay
// groupchat messages are fanned out by the sender as separate one-to-one
// messages, with no relay point, no shared transcript and no sequence numbers -
// so a member can tell one player they folded and another that they raised, and
// the network will never notice. A chain of signed entries is what gives such a
// table a single well-defined history at all, and what turns equivocation from
// an accusation into a two-message proof anyone can check.
//
// The package deliberately imports nothing from the gRPC layer. It has to
// outlive that transport, and the same bytes have to verify identically on
// every implementation, so its encoding is its own.
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

// Version is the entry format version. It is covered by the signature, so a
// verifier can never be talked into reading one version's bytes as another's.
//
// Two since entries began carrying the block height their author saw.
const Version uint16 = 2

const (
	// PubKeyLen is the length of a compressed secp256k1 public key.
	PubKeyLen = 33

	// SigLen is the length of a consensus Schnorr signature. Unlike the
	// escrow signatures there is no trailing hash-type byte: nothing here
	// is spending an output.
	SigLen = 64

	// maxActionLen bounds the action label so a malformed entry cannot make
	// the canonical encoding unbounded.
	maxActionLen = 16
)

// entryTag domain-separates entry hashes. Without it a signature over an entry
// could be replayed anywhere else the same key signs a 32-byte digest.
var entryTag = []byte("dcrpoker/gamelog/entry/v1")

// headTag domain-separates head attestations from entries, so an attestation
// can never be read as the entry it attests to.
var headTag = []byte("dcrpoker/gamelog/head/v1")

// matchTag domain-separates the genesis hash of a chain.
var matchTag = []byte("dcrpoker/gamelog/match/v1")

// Action is what a seat did. It is a string rather than an enum so a transcript
// stays readable, and so the existing hand-history schema in the server's
// database - which stores the action as text - can record it unchanged.
type Action string

const (
	ActionFold  Action = "fold"
	ActionCheck Action = "check"
	ActionCall  Action = "call"
	ActionBet   Action = "bet"
	ActionRaise Action = "raise"
	ActionAllIn Action = "allin"
)

// Valid reports whether a is an action this package knows how to record.
func (a Action) Valid() bool {
	switch a {
	case ActionFold, ActionCheck, ActionCall, ActionBet, ActionRaise, ActionAllIn:
		return true
	}
	return false
}

// Street is the betting round an action was taken on. It is a domain type on
// purpose: the engine's phase enum comes from the generated protobuf package,
// and depending on that here would tie the log to a transport it is meant to
// outlive.
type Street uint8

const (
	StreetPreFlop Street = 0
	StreetFlop    Street = 1
	StreetTurn    Street = 2
	StreetRiver   Street = 3
)

func (s Street) Valid() bool { return s <= StreetRiver }

func (s Street) String() string {
	switch s {
	case StreetPreFlop:
		return "preflop"
	case StreetFlop:
		return "flop"
	case StreetTurn:
		return "turn"
	case StreetRiver:
		return "river"
	}
	return fmt.Sprintf("street(%d)", uint8(s))
}

// Entry is one action, signed by the seat that took it and chained to the entry
// before it.
type Entry struct {
	Version  uint16
	PrevHash [32]byte
	Seq      uint64
	Hand     uint64
	Street   Street
	Seat     uint32
	Signer   []byte // 33-byte compressed session pubkey
	Action   Action
	Amount   int64
	// Height is the block height this entry's author saw when it acted.
	//
	// Recorded, not enforced, and the difference matters. A verifier can
	// check that heights do not go backwards along the chain and nothing
	// else: a peer that is genuinely behind and one that is lying about
	// being behind look identical, and there is no construction that
	// separates them. What it buys is a record of tempo - how long a table
	// took over a hand, and which seat it waited on - which is what
	// reputation needs and what a claim needs to judge how long an
	// obligation has stood.
	//
	// It is emphatically not a turn timer. Blocks average five minutes and
	// arrive in a Poisson process, so nothing derived from them is fast
	// enough for poker, and money moving on a clock is money moving on one
	// machine's opinion.
	Height uint32
	Sig    []byte // 64-byte Schnorr signature over Hash
}

// SigningBytes is the canonical encoding an entry's signature covers.
//
// The layout is fixed and explicit rather than derived from a struct encoder.
// Two implementations have to agree byte for byte or every signature between
// them is worthless, and general-purpose encodings leave far too much room to
// disagree - field order, integer width, map iteration, how a string is
// escaped. Nothing here is negotiable at runtime.
func (e *Entry) SigningBytes() ([]byte, error) {
	if err := e.checkShape(); err != nil {
		return nil, err
	}

	var b bytes.Buffer
	b.Write(entryTag)
	_ = binary.Write(&b, binary.BigEndian, e.Version)
	b.Write(e.PrevHash[:])
	_ = binary.Write(&b, binary.BigEndian, e.Seq)
	_ = binary.Write(&b, binary.BigEndian, e.Hand)
	b.WriteByte(byte(e.Street))
	_ = binary.Write(&b, binary.BigEndian, e.Seat)
	b.Write(e.Signer) // fixed 33 bytes, checked above
	b.WriteByte(byte(len(e.Action)))
	b.WriteString(string(e.Action))
	_ = binary.Write(&b, binary.BigEndian, e.Amount)
	_ = binary.Write(&b, binary.BigEndian, e.Height)
	return b.Bytes(), nil
}

// Hash is the digest an entry is signed over and the next entry chains to.
func (e *Entry) Hash() ([32]byte, error) {
	msg, err := e.SigningBytes()
	if err != nil {
		return [32]byte{}, err
	}
	return blake256.Sum256(msg), nil
}

// Sign fills in Signer and Sig for the given log key.
//
// The key is a forfeit.LogKey rather than a bare private key, and that is the
// whole difference between a log that can prove cheating and one that can
// punish it. The nonce comes from the position in the log rather than from the
// message, so signing one entry per sequence number reveals nothing and signing
// two publishes the key - see pkg/forfeit. Equivocating is no longer something
// somebody has to notice, report and be believed about; it hands the key to the
// player who was lied to.
//
// A plain schnorr.Sign here would still produce entries that verify, still
// produce EquivocationProofs, and still leave the cheat with nothing to lose.
func (e *Entry) Sign(key *forfeit.LogKey) error {
	if key == nil {
		return fmt.Errorf("no signing key")
	}
	e.Signer = key.Public().SerializeCompressed()
	h, err := e.Hash()
	if err != nil {
		return err
	}
	sig, err := key.Sign(forfeit.DomainEntry, e.Seq, h[:])
	if err != nil {
		return fmt.Errorf("sign entry: %w", err)
	}
	e.Sig = sig
	return nil
}

// Verify checks the entry's signature against the key it names. It says nothing
// about whether the entry belongs in any particular chain - see Chain.Append.
func (e *Entry) Verify() error {
	h, err := e.Hash()
	if err != nil {
		return err
	}
	if len(e.Sig) != SigLen {
		return fmt.Errorf("signature is %d bytes, want %d", len(e.Sig), SigLen)
	}
	pub, err := secp256k1.ParsePubKey(e.Signer)
	if err != nil {
		return fmt.Errorf("signer key: %w", err)
	}
	sig, err := schnorr.ParseSignature(e.Sig)
	if err != nil {
		return fmt.Errorf("parse signature: %w", err)
	}
	if !sig.Verify(h[:], pub) {
		return fmt.Errorf("signature does not verify for seat %d", e.Seat)
	}
	return nil
}

func (e *Entry) checkShape() error {
	if e.Version != Version {
		return fmt.Errorf("entry version %d, want %d", e.Version, Version)
	}
	if !e.Street.Valid() {
		return fmt.Errorf("unknown street %d", uint8(e.Street))
	}
	if !e.Action.Valid() {
		return fmt.Errorf("unknown action %q", e.Action)
	}
	if len(e.Action) > maxActionLen {
		return fmt.Errorf("action label is %d bytes, over the %d byte limit", len(e.Action), maxActionLen)
	}
	if len(e.Signer) != PubKeyLen {
		return fmt.Errorf("signer key is %d bytes, want %d", len(e.Signer), PubKeyLen)
	}
	if e.Amount < 0 {
		return fmt.Errorf("amount %d is negative", e.Amount)
	}
	// Only wagering actions carry money. A fold for 100 atoms would be a
	// contradiction the rest of the system might read either way.
	switch e.Action {
	case ActionFold, ActionCheck:
		if e.Amount != 0 {
			return fmt.Errorf("%s carries an amount of %d", e.Action, e.Amount)
		}
	}
	return nil
}

// GenesisHash is what the first entry of a match chains to. Deriving it from
// the match id means a chain is bound to its table: an entry signed for one
// match can never be replayed into another, because the hash it chains back to
// would have to collide.
func GenesisHash(matchID string) [32]byte {
	var b bytes.Buffer
	b.Write(matchTag)
	b.WriteString(matchID)
	return blake256.Sum256(b.Bytes())
}
