package runtime

import (
	"context"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/ruling"
)

// Chain is what a game may read about the chain, and no more.
//
// Enough to run its own duty clocks - a deadline is a height, and a height is
// something the game must be able to compare against - and not enough to build
// or broadcast anything. A game that could broadcast could pay somebody.
type Chain struct {
	// Height is the tip.
	Height int64
	// Hash is the tip's block hash, hex.
	Hash string
}

// Entry is one record from the signed log of a table, as a game reads it.
//
// The log is the runtime's to keep and hash-chain. A game reads it to decide
// whether a duty was discharged; it never writes one directly, because an entry
// that skipped the chain would be an entry nobody could prove.
type Entry struct {
	Seq  uint64
	Seat uint32
	Kind schema.Kind
	Body []byte
}

// Game is the runtime as a game sees it: the things a game asks for, rather
// than the things it is asked.
//
// An interface rather than a struct so a game's own tests can stand in for it
// without a bridge, which is the other half of shipping bridgetest.
type Game interface {
	// Send puts a message to the table. The runtime frames, chunks and
	// routes it; the game decides what is in it.
	Send(ctx context.Context, match string, kind schema.Kind, body any) error

	// Settle declares who won, and the runtime builds and co-signs the
	// payout from the table's own terms.
	//
	// The runtime does not check the outcome, because it has no way to: the
	// rules that produced it are the game's. What it does check is that the
	// payout it builds from that outcome matches the terms every seat signed.
	Settle(ctx context.Context, match string, out Outcome) error

	// Forfeit carries out a forfeiture the game has ruled on.
	//
	// An equivocation ruling is verified here before anything is spent - the
	// key either falls out of the two signatures or it does not. A silence
	// ruling is taken at the game's word, because the ladder gives the accused
	// an on-chain right of reply and the SDK has no vocabulary for what was
	// owed. See pkg/ruling.
	Forfeit(ctx context.Context, r ruling.Ruling) error

	// Reclaim pulls this seat's own locked money home once its timelock has
	// matured. Safe to call early; it reports what is not yet claimable
	// rather than trying.
	Reclaim(ctx context.Context, match string) error

	// Chain is the tip, for a game running its own duty clocks.
	Chain(ctx context.Context) (Chain, error)

	// Log is the table's signed log so far, for the same reason.
	Log(match string) []Entry

	// Seats is the roster once a table has formed.
	Seats(match string) (map[uint32][]byte, bool)
}

// Outcome is how a finished table's money is divided.
//
// Named shares rather than a winner because a draw, a split pot and a
// three-way are all outcomes a game may reach, and a runtime that only
// understood "winner" would push every game into pretending.
type Outcome struct {
	// Shares is how much each seat is paid, in atoms, and must sum to what
	// the table holds less the fee.
	Shares map[uint32]int64
	// Void says the table is unwound rather than paid out: every seat takes
	// its own stake back. A game reaching an outcome nobody won says so here
	// rather than inventing an even split.
	Void bool
}
