package schema

import "fmt"

// DutyKind is the sort of thing a seat can owe.
type DutyKind string

const (
	// DutyCardKey is a deck key for the hand being set up.
	DutyCardKey DutyKind = "cardkey"
	// DutyShuffle is this seat's turn to permute the deck.
	DutyShuffle DutyKind = "shuffle"
	// DutyShare is a decryption share the rest of the table is waiting on.
	DutyShare DutyKind = "share"
	// DutyAction is this seat's turn to act.
	DutyAction DutyKind = "action"
	// DutyCheckpoint is a signature over the stacks at a hand boundary.
	DutyCheckpoint DutyKind = "checkpoint"
	// DutyReveal is a challenged hand's secrets. Owed by every seat the
	// moment any seat challenges, discharged by revealing.
	//
	// A disputed shuffle, by contrast, obliges nobody: the complaint
	// carries the refused frame whole, so every peer reaches the verdict
	// from the complaint alone and there is nothing left for the accused
	// to do or refuse. No duty, no window, no claim.
	DutyReveal DutyKind = "reveal"
)

// Duty is one thing the log says a seat still has to do.
//
// At means whatever the Kind says it does - the shuffling round, the deck slot,
// the sequence number, and nothing at all for a card key or a checkpoint, which
// are identified by their hand.
type Duty struct {
	Seat int      `json:"seat"`
	Kind DutyKind `json:"kind"`
	Hand uint64   `json:"hand"`
	At   uint64   `json:"at"`
}

func (d Duty) String() string {
	switch d.Kind {
	case DutyCardKey:
		return fmt.Sprintf("seat %d owes a card key for hand %d", d.Seat, d.Hand)
	case DutyShuffle:
		return fmt.Sprintf("seat %d owes shuffle %d of hand %d", d.Seat, d.At, d.Hand)
	case DutyShare:
		return fmt.Sprintf("seat %d owes a share for slot %d of hand %d", d.Seat, d.At, d.Hand)
	case DutyAction:
		return fmt.Sprintf("seat %d owes the entry at sequence %d", d.Seat, d.At)
	case DutyCheckpoint:
		return fmt.Sprintf("seat %d owes a checkpoint for hand %d", d.Seat, d.Hand)
	case DutyReveal:
		return fmt.Sprintf("seat %d owes its deck secrets for challenged hand %d", d.Seat, d.Hand)
	}
	return fmt.Sprintf("seat %d owes %q", d.Seat, d.Kind)
}
