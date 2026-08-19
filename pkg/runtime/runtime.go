package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/decred/slog"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
	"github.com/karamble/dcrgaming-sdk/pkg/identity"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

// ErrNotYet is returned by a stage the runtime does not carry out yet.
//
// The lifecycle is built as one slim end-to-end path first and filled in stage
// by stage, so a game can plug in and be told plainly which stage is still
// missing rather than discovering it as a nil dereference.
var ErrNotYet = errors.New("this stage of the lifecycle is not built yet")

// Config is everything the runtime needs to run a game.
type Config struct {
	// Rules is the game. Required.
	Rules Rules
	// Bridge is a dialled connection. Required. The runtime does not dial:
	// how a game finds its bridge is connect's business and the game's.
	Bridge *transport.Bridge
	// Book is where money in flight is recorded. Required - a runtime with
	// nowhere to write a request down is a runtime that can lose a payment.
	Book *spend.Book
	// Identity is the game's seed, from which every seat key is derived.
	// Required.
	Identity *identity.Identity
	// SeatTags are the game's own domain-separation tags for those keys.
	// Required, and the game's to state: they are frozen hash inputs that
	// decide which keys a seat has, so the SDK must not invent them.
	SeatTags identity.SeatTags
	// Log is optional.
	Log slog.Logger
}

// Runtime owns the table lifecycle: invite, seat, fund, settle, forfeit,
// reclaim.
//
// A game hands it [Rules] and calls [Game] methods; it never drives.
type Runtime struct {
	rules    Rules
	bridge   *transport.Bridge
	book     *spend.Book
	identity *identity.Identity
	seatTags identity.SeatTags
	log      slog.Logger

	mu     sync.Mutex
	router *transport.Router
	tables map[string]*table
	payout string
	names  map[string]string
}

// table is one table's runtime state. Seating fills it in; the stages read it.
type table struct {
	match string
	gcID  string
	form  *membership.Formation
	seats map[uint32][]byte
	log   []Entry
}

// New builds a runtime. It does not start anything; call [Runtime.Run].
func New(cfg Config) (*Runtime, error) {
	if cfg.Rules == nil {
		return nil, fmt.Errorf("a runtime needs a game to run")
	}
	if cfg.Bridge == nil {
		return nil, fmt.Errorf("a runtime needs a dialled bridge")
	}
	if cfg.Book == nil {
		return nil, fmt.Errorf("a runtime needs a spend book; money in flight has to be written down")
	}
	if cfg.Identity == nil {
		return nil, fmt.Errorf("a runtime needs the game's identity to derive seat keys from")
	}
	if cfg.SeatTags.Session == "" || cfg.SeatTags.Log == "" || cfg.SeatTags.Bond == "" {
		return nil, fmt.Errorf("a runtime needs the game's three seat-key tags; they are frozen inputs and the SDK must not invent them")
	}
	if err := cfg.Rules.Identity().Validate(); err != nil {
		return nil, fmt.Errorf("the game does not introduce itself: %w", err)
	}
	log := cfg.Log
	if log == nil {
		log = slog.Disabled
	}
	return &Runtime{
		rules: cfg.Rules, bridge: cfg.Bridge, book: cfg.Book, log: log,
		identity: cfg.Identity, seatTags: cfg.SeatTags,
		tables: map[string]*table{},
		names:  map[string]string{},
	}, nil
}

// Run owns the loop until the context ends.
//
// Two things run: the bridge's control requests, and the frames addressed to
// this game. Both stop when the context does, and Run returns the first error
// that is not simply the context ending.
func (r *Runtime) Run(ctx context.Context) error {
	frames, err := r.bridge.Events(ctx)
	if err != nil {
		return fmt.Errorf("subscribe to the bridge: %w", err)
	}

	errs := make(chan error, 2)
	go func() { errs <- r.serveRequests(ctx) }()
	go func() { errs <- r.route(ctx, frames) }()

	first := <-errs
	if first != nil && !errors.Is(first, context.Canceled) {
		return first
	}
	return nil
}

// route feeds the bridge's frames through the router, which is what does the
// framing, the chunk reassembly, the sender check and the other-game filtering.
//
// Deliberately not a hand-rolled decode: that machinery is already written and
// tested in transport and wire, and it is exactly the kind of thing that looks
// simple until a chunked message arrives out of order.
func (r *Runtime) route(ctx context.Context, frames <-chan transport.InboundFrame) error {
	id := r.rules.Identity()
	router, err := transport.NewRouter(transport.Config{
		Game:    id.GameID,
		GameVer: int(id.GameVer),
		Sender:  r.bridge,
		Log:     r.log,
		// A sender may talk to a table once they are on its roster. Before
		// a table has formed nobody is authorised, which is what stops a
		// stranger allocating state at a table they are not in.
		Authorize: r.authorized,
		Handle: func(d transport.Delivery) {
			msg := Message{Match: d.SID, GCID: d.GCID, From: d.Sender}
			if d.Msg != nil {
				msg.Kind, msg.Body = d.Msg.Kind, d.Msg.Body
			}
			if err := r.rules.Handle(ctx, msg); err != nil {
				r.log.Warnf("the game refused a %s message: %v", msg.Kind, err)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("build the router: %w", err)
	}
	r.mu.Lock()
	r.router = router
	r.mu.Unlock()

	transport.Receive(ctx, frames, router)
	return ctx.Err()
}

// Chain is the tip, for a game running its own duty clocks.
func (r *Runtime) Chain(ctx context.Context) (Chain, error) {
	tip, err := r.bridge.ChainTip(ctx)
	if err != nil {
		return Chain{}, err
	}
	return Chain{Height: tip.Height, Hash: tip.Hash}, nil
}

// Log is a table's signed log so far.
func (r *Runtime) Log(match string) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tables[match]
	if !ok {
		return nil
	}
	return append([]Entry(nil), t.log...)
}

// Seats is a table's roster once it has formed.
func (r *Runtime) Seats(match string) (map[uint32][]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tables[match]
	if !ok || len(t.seats) == 0 {
		return nil, false
	}
	out := make(map[uint32][]byte, len(t.seats))
	for k, v := range t.seats {
		out[k] = append([]byte(nil), v...)
	}
	return out, true
}

// Payout is the address the operator set for winnings, if they have set one.
func (r *Runtime) Payout() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.payout
}

// Names is the operator's names for the identities at the table.
func (r *Runtime) Names() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.names))
	for k, v := range r.names {
		out[k] = v
	}
	return out
}

// Book is the record of money in flight, for a game that wants to read it.
// Reading is supported; writing to it behind the runtime's back is not.
func (r *Runtime) Book() *spend.Book { return r.book }
