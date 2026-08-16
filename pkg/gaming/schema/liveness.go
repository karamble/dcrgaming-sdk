package schema

import (
	"encoding/hex"
	"fmt"

	"github.com/karamble/dcrgaming-sdk/pkg/gamelog"
)

// Checkpoint is a seat's signature over the stacks at the end of a hand.
//
// Every seat sends one at every hand boundary, and a peer keeps the newest set
// it holds signed by everybody. That set is what a table settles on if it stops:
// the hand under way was never agreed - one seat is mid-decision and the others
// have not seen it - so there is nothing to settle it by, and it is simply void.
//
// Which is also why nobody has to work out who walked off. The settlement is the
// same transaction whoever broadcasts it, so there is no accusation to make and
// nothing for a liar to gain by making one.
type Checkpoint struct {
	Hand   uint64  `json:"hand"`
	Stacks []int64 `json:"stacks"`
	Seat   uint32  `json:"seat"`
	Signer string  `json:"signer"` // hex compressed log pubkey
	Sig    string  `json:"sig"`
}

// CheckpointFrom renders a checkpoint for the wire.
func CheckpointFrom(c *gamelog.Checkpoint) Checkpoint {
	if c == nil {
		return Checkpoint{}
	}
	return Checkpoint{
		Hand:   c.Hand,
		Stacks: append([]int64(nil), c.Stacks...),
		Seat:   c.Seat,
		Signer: hex.EncodeToString(c.Signer),
		Sig:    hex.EncodeToString(c.Sig),
	}
}

// Into reads a checkpoint back.
//
// The signature is not checked here. That is gamelog's job and it needs the
// roster to do it - a checkpoint that verifies against a key nobody at the table
// holds is worth exactly nothing, and checking it here without the roster would
// look like a check while being none.
func (c Checkpoint) Into() (*gamelog.Checkpoint, error) {
	signer, err := hex.DecodeString(c.Signer)
	if err != nil {
		return nil, fmt.Errorf("checkpoint signer: %w", err)
	}
	sig, err := hex.DecodeString(c.Sig)
	if err != nil {
		return nil, fmt.Errorf("checkpoint signature: %w", err)
	}
	if len(c.Stacks) == 0 {
		return nil, fmt.Errorf("a checkpoint carries no stacks")
	}
	return &gamelog.Checkpoint{
		Hand:   c.Hand,
		Stacks: append([]int64(nil), c.Stacks...),
		Seat:   c.Seat,
		Signer: signer,
		Sig:    sig,
	}, nil
}

// Claim proposes taking a bond from a seat that is holding the table up, for the
// other seats to co-sign.
//
// It names an obligation rather than a person, and that is the whole of its
// safety. A claim that said only "this player is gone" could be opened against
// anybody at any time - and it was worse than useless, because a player who
// stalls could wait for their opponent to give up and then claim the bond of the
// person who stopped playing because of them. Heads-up there is nobody else to
// refuse it.
//
// So it names what the log says the accused owes, and every co-signer checks
// that against its own copy before signing (driver.Table.Agrees). A seat that
// owes nothing cannot be claimed against, which is exactly the case a staller
// needs: while they are the one holding things up, it is their turn, so the
// player waiting on them owes nothing.
//
// What the message still does not do is decide anything. Whether the accused is
// really gone is settled by the on-chain race - the claim is delayed and the
// accused answers by spending the same output - not by anyone reading this. A
// peer that disagrees simply does not sign, and one that never received it loses
// nothing.
type Claim struct {
	// Seat is the one being claimed against.
	Seat uint32 `json:"seat"`
	// Duty is what the log says that seat owes: its kind, the hand, and the
	// position within it. A co-signer that reads a different obligation from
	// its own log refuses, including when it is merely behind - refusing
	// under uncertainty costs a retry, signing under it costs a bond.
	Duty Duty `json:"duty"`
	// Bond is the outpoint holding that seat's table bond, and the script
	// it is locked behind. The script travels because it is what says which
	// roster can take the coin, and a peer must check it names them before
	// putting a signature to anything.
	BondOutpoint string `json:"bondOutpoint"`
	BondScript   string `json:"bondScript"`
	// Tx is the unsigned claim transaction, hex-encoded, so every co-signer
	// puts their name to the same bytes rather than to a description of
	// them.
	Tx string `json:"tx"`
	// Sig is this peer's signature over its input, if it has signed.
	Signer string `json:"signer,omitempty"` // hex compressed session pubkey
	Sig    string `json:"sig,omitempty"`
}

// Validate reports whether a claim could be acted on at all.
//
// Shape only. Whether the bond really names this table, and whether the
// transaction really pays where it should, are checked against the script and
// the roster by whoever is deciding to sign - not here, where the roster is not
// known.
func (c Claim) Validate() error {
	if c.BondOutpoint == "" {
		return fmt.Errorf("a claim names no bond deposit")
	}
	if _, err := hex.DecodeString(c.BondScript); err != nil || c.BondScript == "" {
		return fmt.Errorf("a claim carries no bond script")
	}
	if _, err := hex.DecodeString(c.Tx); err != nil || c.Tx == "" {
		return fmt.Errorf("a claim carries no transaction to sign")
	}
	if (c.Signer == "") != (c.Sig == "") {
		return fmt.Errorf("a claim carries a signature without a signer, or the reverse")
	}
	if c.Duty.Kind == "" {
		return fmt.Errorf("a claim names no obligation, so there is nothing to check it against")
	}
	if c.Duty.Seat != int(c.Seat) {
		return fmt.Errorf("a claim against seat %d names an obligation of seat %d",
			c.Seat, c.Duty.Seat)
	}
	return nil
}

// Leaving is a seat saying it is getting up.
//
// Distinct from going quiet, which is the point of having it. A player who says
// this folds the hand they are in and the table settles at its next boundary; a
// player who simply stops is answered by a claim on their bond. Without a way to
// say it, the only exit from a table was the one that looks exactly like the
// thing that gets punished - which left anybody stuck opposite a slow player
// with no move at all.
type Leaving struct {
	Seat uint32 `json:"seat"`
	Hand uint64 `json:"hand"`
	// Sig is the seat saying this is its own decision. Once every seat is
	// marked leaving the table settles, so an unsigned one would let anybody
	// who could reach the table end somebody else's game.
	Sig string `json:"sig"`
}

// Accusation is one member's signature on a future accusation against a seat.
//
// Agreed while the table is still cooperating, because an accusation needs every
// member's signature including the accused's, and the accused would not give it
// once it is being accused. It pays into the claimed bond, where its target has
// the window to answer with its own key alone - so what is gathered here is the
// ability to accuse, and losing it costs nobody their defence.
type Accusation struct {
	// Seat is whose bond this accuses.
	Seat uint32 `json:"seat"`
	// Tx is the unsigned answer, hex-encoded, so everybody signs the same
	// bytes rather than a description of them.
	Tx     string `json:"tx"`
	Signer string `json:"signer"` // hex compressed session pubkey
	Sig    string `json:"sig"`
}

// Settle is one member's signature on the transaction that pays a table out.
//
// One signature per input, because each seat's stake sits behind its own script
// and the settlement branch of every one of them names the whole table.
type Settle struct {
	// Hand is the checkpoint this pays out at, so a signature for one
	// boundary cannot be applied to another.
	Hand uint64 `json:"hand"`
	// Tx is the unsigned settlement, hex-encoded.
	Tx     string   `json:"tx"`
	Signer string   `json:"signer"` // hex compressed session pubkey
	Sigs   []string `json:"sigs"`   // one per input, in input order
}

// HeadFrom renders a seat's claim about the log head.
//
// The claim exists so two peers can find out they disagree without either
// sending its whole history. A sequence number and a hash is enough: the peer
// that is behind is repaired by being sent what it lacks, and two different
// hashes at one sequence is a fork, which is a different problem entirely and
// is what makes this worth signing.
func HeadFrom(h *gamelog.HeadAttestation) Head {
	if h == nil {
		return Head{}
	}
	return Head{
		Seq:    h.Seq,
		Hash:   hex.EncodeToString(h.Hash[:]),
		Seat:   h.Seat,
		Signer: hex.EncodeToString(h.Signer),
		Sig:    hex.EncodeToString(h.Sig),
	}
}

// Into reads a head claim back. It does not verify it; that needs the roster.
func (h Head) Into() (*gamelog.HeadAttestation, error) {
	hash, err := hex.DecodeString(h.Hash)
	if err != nil {
		return nil, fmt.Errorf("head hash: %w", err)
	}
	if len(hash) != 32 {
		return nil, fmt.Errorf("head hash is %d bytes, want 32", len(hash))
	}
	signer, err := hex.DecodeString(h.Signer)
	if err != nil {
		return nil, fmt.Errorf("head signer: %w", err)
	}
	sig, err := hex.DecodeString(h.Sig)
	if err != nil {
		return nil, fmt.Errorf("head signature: %w", err)
	}
	att := &gamelog.HeadAttestation{Seq: h.Seq, Seat: h.Seat, Signer: signer, Sig: sig}
	copy(att.Hash[:], hash)
	return att, nil
}

// Release is one member's signature on the transaction that hands a seat's
// table bond back.
//
// One signature and one input, unlike a settlement: each bond is its own output
// under its own script, and the branch that releases it names the whole table.
// So a table of n seats releases n bonds as n transactions rather than one, and
// every member signs each of them.
type Release struct {
	// Seat is whose bond this releases. A signature for one seat's bond
	// cannot be applied to another's, and saying which is what lets a peer
	// rebuild the transaction it is being asked to sign.
	Seat uint32 `json:"seat"`
	// Tx is the unsigned release, hex-encoded.
	Tx     string `json:"tx"`
	Signer string `json:"signer"` // hex compressed session pubkey
	Sig    string `json:"sig"`
}

// Take is one member's signature on taking a claimed bond whose window has closed.
//
// Not agreed in advance, unlike an accusation: the seats taking it are all willing
// at the time, which is what being the accusers means. Proposed, co-signed and
// broadcast, and the only condition on it is one the chain states.
type Take struct {
	// Seat is whose bond is being taken, and Outpoint is the claimed bond
	// holding it.
	Seat     uint32 `json:"seat"`
	Outpoint string `json:"outpoint"`
	Claimed  string `json:"claimed"` // hex claimed bond script
	Tx       string `json:"tx"`      // hex unsigned transaction
	Signer   string `json:"signer,omitempty"`
	Sig      string `json:"sig,omitempty"`
}
