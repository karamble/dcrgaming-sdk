// Package runtime is the lifecycle a game plugs into: invite, seat, fund,
// settle, forfeit, reclaim.
//
// The shape is inverted from how both existing games are written. They are
// drivers - each owns its loop, dials the bridge, dispatches the bridge's
// control requests, keeps its own book of money in flight, and calls the SDK
// for scripts and keys along the way. That is why they ended up with 61
// identically-named functions between them: everything above the primitives had
// to be written twice, and the second time from memory.
//
// Here the runtime owns the loop and the game implements [Rules]. A game says
// what it is called, what a table's terms are, what to do with a message
// addressed to it, and what its state is. It never writes a spend book, a
// bridge dispatcher, a bond ladder, a sweeper or a seating machine, and it never
// sees an HMAC tag, an escrow script or a gRPC call.
//
// # What the game pushes back
//
// Two things flow the other way, and both are decisions only the game can make:
//
//   - An outcome. The runtime does not know who won.
//   - A ruling. The runtime does not know who cheated. See [pkg/ruling] - the
//     SDK executes a forfeiture, it never decides one.
//
// Those are methods on the runtime rather than on Rules, because they happen
// when the game's rules say so and not when the runtime asks.
//
// # What the game reads
//
// A game watches its own duty clocks, which is a deliberate boundary: a
// deadline passing is chain state, but *what was owed* is game logic and the SDK
// has no vocabulary for it. So the runtime exposes the chain tip and the signed
// log as a read surface and leaves the deciding alone.
package runtime

import (
	"context"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/connect"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

// Rules is everything a game has to provide. Four methods.
//
// Deliberately small, and deliberately without a method for anything the
// runtime can work out for itself. Every method here is one the runtime cannot
// answer: what this game is called, what a table costs, what a message means,
// and what the operator should be shown.
type Rules interface {
	// Identity introduces the game to the bridge.
	//
	// Called once at startup. The bridge checks what it says rather than
	// believing it.
	Identity() connect.Identity

	// Terms states the money terms for a session the game is joining or
	// hosting: buy-in, seats, refund lock, bond, admission deadline.
	//
	// Called before a table forms. Returning an error refuses the table,
	// which is the right answer for an invite whose terms this game will not
	// play under.
	Terms(sid string) (membership.Terms, error)

	// Handle receives one message addressed to this game.
	//
	// The runtime has already framed, routed, reassembled and checked the
	// sender by then; the body is whatever the game put there and the runtime
	// has not looked inside it. Errors are logged and the message dropped -
	// a game that wants a message redelivered has to ask for it, because a
	// runtime that retried on the game's behalf would replay moves.
	Handle(ctx context.Context, in Message) error

	// State is what the dashboard shows an operator, answered on demand when
	// the bridge asks.
	//
	// Called from the bridge's request loop, so it must not block on the
	// game's own locks for long and must not call back into the runtime.
	State(ctx context.Context) State
}

// Message is one frame addressed to this game.
type Message struct {
	// Match is the table it belongs to, and GCID the group chat it arrived
	// through.
	Match string
	GCID  string
	// From is the sender's authenticated identity, taken from the channel
	// and the envelope and never from anything inside the body.
	From []byte
	// Kind is the game's own word for what this message is.
	Kind schema.Kind
	// Body is what the game encoded. The runtime has not read it.
	Body []byte
}

// State is a game's own summary, for an operator to read.
//
// Free-form on purpose: a dashboard shows it and nothing branches on it, so
// pinning a schema here would be inventing a vocabulary for games that do not
// exist yet.
type State struct {
	// Summary is one line: what this game is doing right now.
	Summary string
	// Tables is one entry per table the game has open.
	Tables []TableState
}

// TableState is what an operator is shown about one table.
type TableState struct {
	Match  string
	Status string
	Seats  uint32
	// Detail is the game's own extra fields, shown as given.
	Detail map[string]string
}

// Seated is an optional hook. A game that implements it is told when a table
// has finished forming, which is when it may start play.
type Seated interface {
	Seated(ctx context.Context, match string, seats map[uint32][]byte)
}

// Settled is an optional hook. A game that implements it is told when a
// settlement has been broadcast, which is when the money is decided.
type Settled interface {
	Settled(ctx context.Context, match string, txid string)
}
