package runtime

import (
	"context"

	"github.com/karamble/dcrgaming-sdk/pkg/evidence"
	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/wire"
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

// Game is the runtime as a game sees it: the things a game asks for, rather
// than the things it is asked.
//
// An interface rather than a struct so a game's own tests can stand in for it
// without a bridge, which is the other half of shipping bridgetest.
type Game interface {
	// Send puts a message to the table. The runtime frames, chunks and
	// routes it; the game decides what is in it, and how long it is worth
	// delivering - a move in a hand and a piece of evidence do not have the
	// same life.
	Send(ctx context.Context, match string, kind schema.Kind, body any, class wire.Class) error

	// Settle declares who won, and the runtime builds and co-signs the
	// payout from the table's own terms.
	//
	// The runtime does not check the outcome, because it has no way to: the
	// rules that produced it are the game's. What it does check is that the
	// payout it builds from that outcome matches the terms every seat signed,
	// and that it carries the signatures the stake escrow's own script asks
	// for - which today is all of them, but that is the script's statement
	// and not this method's.
	Settle(ctx context.Context, match string, out Outcome) error

	// Seize spends a branch of a seat's forfeitable bond, using the key that
	// seat's own signatures gave up.
	//
	// Nothing here is taken on the game's word, and nothing is verified from
	// the game's evidence either: the bond was derived here from the roster,
	// and a key that opens no branch of it is refused by escrow arithmetic
	// before anything is built. A game whose cheating exposes no key has
	// nothing for this to spend.
	Seize(ctx context.Context, match string, seat uint32, exposed *evidence.Exposed) error

	// Accuse opens the accusation chain against a seat that stopped
	// answering, which takes nothing: it spends the accused's table bond
	// into a claim they have an on-chain window to answer. The game says
	// what was owed and when; the chain says whether it is late.
	Accuse(ctx context.Context, match string, seat uint32, lapsed Lapsed) error

	// Release hands this seat's own table bond back cooperatively, which is
	// what a match ending with nothing to punish comes to.
	Release(ctx context.Context, match string) error

	// Reclaim pulls this seat's own locked money home once its timelock has
	// matured. Safe to call early; it reports what is not yet claimable
	// rather than trying.
	Reclaim(ctx context.Context, match string) error

	// Chain is the tip, for a game running its own duty clocks, and
	// BlockHash is what a past block hashed to, for a game anchoring its
	// own moves in time.
	Chain(ctx context.Context) (Chain, error)
	BlockHash(ctx context.Context, height uint32) (string, error)

	// Seat is which seat at a table is this peer's own.
	Seat(match string) (uint32, bool)

	// Seats is the roster once a table has formed, and LogSeats the key
	// each seat signs its own moves with.
	Seats(match string) (map[uint32][]byte, bool)
	LogSeats(match string) (map[uint32][]byte, bool)

	// LogKey is this seat's own signing key, bound to the table. The one
	// key a game is handed: it signs what the game says happened, where the
	// session key holds the stake and stays with the runtime.
	LogKey(match string) (*forfeit.LogKey, error)

	// MatchID is what the table is called once it has decided who is at it,
	// which is what a game's log and evidence are bound to.
	MatchID(match string) (string, bool)
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
