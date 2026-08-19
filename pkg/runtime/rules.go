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
// Five things flow the other way, and every one of them is a decision only the
// game can make. Four are money, and none of them names a game:
//
//   - [Runtime.Settle], who is paid. The runtime does not know who won.
//   - [Runtime.Seize], spend a branch of a seat's bond that seat's own key
//     opened. The runtime is not told what the cheat was, and there is no kind
//     of cheating for it to have heard of.
//   - [Runtime.Accuse], open a chain against a seat that stopped answering.
//     The game says what was owed, in its own words; the chain says whether it
//     is late.
//   - [Runtime.Release], hand this seat's own table bond back.
//
// The fifth is a refusal rather than an instruction: [CoSigning] lets a game
// withhold its signature, which is the only answer some rulings have. Deciding
// that somebody cheated is a rule and rules are the game's; what a runtime is
// told is which money to move.
//
// The four are methods on the runtime rather than on Rules, because they happen
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
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/connect"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/wire"
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
	From string
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

// CoSigning is an optional hook. A game that implements it is asked before this
// peer adds its signature to a spend that pays another seat.
//
// This is how a game says no with money rather than about it. Not every ruling
// is an instruction: a seat caught cheating, a fault with nobody to name, an
// accusation answered at every rung - none of those has a transaction to build,
// and the whole of the answer is that this peer stops co-signing. A game with
// nothing to say does not implement this and everything is co-signed, which is
// what happened before the hook existed.
//
// Refusing strands nothing. Every pot the runtime builds has a branch its owner
// can spend alone once its lock matures, so withholding costs the other side a
// wait and gains the refuser nothing - which is what makes it a safe thing to
// let a game decide.
//
// Called with no runtime lock held, and it must not call back into the runtime.
type CoSigning interface {
	// WillCoSign reports whether this peer will sign a spend paying seat at
	// this table. Asked once per seat for a release, and for every seat at
	// the table before a payout, because a payout spends all of them.
	WillCoSign(match string, seat uint32) bool
}

// willCoSign asks the game about one seat, and answers yes for a game that does
// not implement the hook.
func (r *Runtime) willCoSign(match string, seat uint32) bool {
	ask, ok := r.rules.(CoSigning)
	if !ok {
		return true
	}
	return ask.WillCoSign(match, seat)
}

// Send puts one of the game's own messages on the table.
//
// The other half of [Rules.Handle], and the whole of how a game speaks: the
// runtime frames it, chunks it, addresses it to the table's group chat and
// signs nothing, because the body is the game's and the runtime has not looked
// inside it.
//
// The class is the game's to choose because only the game knows how long one
// of its messages is worth delivering. A move in a hand is worthless once the
// hand has moved on; a piece of evidence has to outlive the window it is
// argued in. See [wire.Class].
//
// A game cannot send the runtime's own messages. Joins, commits, rosters,
// stakes, payouts, settlements, releases and accusations are how the money is
// arranged, and a game that could forge one could arrange it differently -
// which is why the runtime keeps them rather than offering them.
func (r *Runtime) Send(ctx context.Context, match string, kind schema.Kind, body any, class wire.Class) error {
	if r.ours(kind) {
		return fmt.Errorf(
			"%q is one of the runtime's own messages and is not a game's to send", kind)
	}
	if strings.TrimSpace(string(kind)) == "" {
		return fmt.Errorf("a message needs a kind")
	}
	t, err := r.tableOf(match)
	if err != nil {
		return err
	}
	return r.router.Send(ctx, t.gCID(), t.match, t.match, kind, body, class)
}

// LogKey is the key this seat signs its own moves with, bound to the table.
//
// The one key a game is handed, and the split is deliberate. The session key
// holds the stake and stays here: it signs joins, payouts and releases, and a
// game that held it could move its own money. The log key signs what the game
// says happened, is the game's alone to use, and is expected to become public
// the moment its owner equivocates - that is what makes a lie provable, and it
// is why a table's log key is not the key its escrow is built on.
//
// Bound to the match here rather than by the caller. A log key carries the
// table it may sign for, and one bound to the wrong table signs entries every
// other seat refuses.
func (r *Runtime) LogKey(match string) (*forfeit.LogKey, error) {
	t, err := r.tableOf(match)
	if err != nil {
		return nil, err
	}
	matchID, ok := t.form.RosterHash()
	if !ok {
		return nil, fmt.Errorf("table %q has no settled roster, so nothing can be signed for it yet", match)
	}
	_, logKey, err := r.seatKeys(t.form.Terms().SID)
	if err != nil {
		return nil, err
	}
	return forfeit.LogKeyFrom(logKey, hex.EncodeToString(matchID[:]))
}

// MatchID is the roster hash: what this table is called once it has decided
// who is at it.
//
// A game's own log and evidence are bound to it, so a game cannot use the
// session id for the purpose - two tables could be formed under one session
// and only one of them is the membership the money is bound to.
func (r *Runtime) MatchID(match string) (string, bool) {
	t, err := r.tableOf(match)
	if err != nil {
		return "", false
	}
	matchID, ok := t.form.RosterHash()
	if !ok {
		return "", false
	}
	return hex.EncodeToString(matchID[:]), true
}

// LogSeats is each seat's log public key, which is what checks the entries
// that seat signs.
func (r *Runtime) LogSeats(match string) (map[uint32][]byte, bool) {
	t, err := r.tableOf(match)
	if err != nil {
		return nil, false
	}
	return t.form.LogSeats()
}
