package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
	"github.com/decred/slog"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
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
	// Params is the chain the game is playing on. Required: every script
	// and address is built against it.
	Params stdaddr.AddressParams
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
	params   stdaddr.AddressParams
	router   *transport.Router
	log      slog.Logger

	// sweepMu guards what this process has broadcast a spend of. Its own
	// lock because it is consulted from the reclaim path while the table
	// lock is not held.
	sweepMu  sync.Mutex
	sweeping map[string]bool

	// runCtx is the context Run was given, so a message the router delivers
	// carries the same lifetime as the loop that fetched it.
	runCtx context.Context

	mu     sync.Mutex
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

	// funded is where each seat's stake landed, filled in by the funding
	// stage. payouts is where each seat asked to be paid, which every seat
	// announces because it goes into the settlement they all sign.
	funded  map[uint32]staked
	payouts map[uint32][]byte

	// settle gathers signatures on this table's payout.
	settle *settlement
}

// settlement is one table's payout, part-signed.
type settlement struct {
	tx    *wire.MsgTx
	draft escrow.SettleDraft
	sigs  map[string][][]byte // by signer's compressed session pubkey, hex
	done  bool
}

// staked is one seat's stake on the chain.
type staked struct {
	outpoint string
	atoms    int64
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
	if cfg.Params == nil {
		return nil, fmt.Errorf("a runtime needs the chain it is playing on")
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
	r := &Runtime{
		rules: cfg.Rules, bridge: cfg.Bridge, book: cfg.Book, log: log,
		identity: cfg.Identity, seatTags: cfg.SeatTags, params: cfg.Params,
		tables: map[string]*table{},
		names:  map[string]string{},
		runCtx: context.Background(),
	}
	// Built here rather than when Run starts, so a game that acts before the
	// loop is up finds a router instead of a race.
	id := cfg.Rules.Identity()
	router, err := transport.NewRouter(transport.Config{
		Game:    id.GameID,
		GameVer: int(id.GameVer),
		Sender:  cfg.Bridge,
		Log:     log,
		// A sender may allocate state for a table this game is at, and no
		// other. See Runtime.authorized for why the sender itself is not
		// checked here.
		Authorize: r.authorized,
		Handle:    r.deliver,
	})
	if err != nil {
		return nil, fmt.Errorf("build the router: %w", err)
	}
	r.router = router
	return r, nil
}

// deliver takes one decoded message: the runtime's own if it is one, the
// game's otherwise.
//
// The runtime does not look inside a game's body and does not retry: a
// redelivered move is a replayed move, so a game that wants one has to ask.
func (r *Runtime) deliver(d transport.Delivery) {
	msg := Message{Match: d.SID, GCID: d.GCID, From: d.Sender}
	if d.Msg != nil {
		msg.Kind, msg.Body = d.Msg.Kind, d.Msg.Body
	}
	if r.ours(msg.Kind) {
		if err := r.handleOurs(r.runCtx, msg); err != nil {
			r.log.Warnf("a %s message was not taken: %v", msg.Kind, err)
		}
		return
	}
	if err := r.rules.Handle(r.runCtx, msg); err != nil {
		r.log.Warnf("the game refused a %s message: %v", msg.Kind, err)
	}
}

// ours reports whether a message kind belongs to the runtime rather than the
// game.
//
// These are the lifecycle's own traffic. A game that used one of these names
// for its own purposes would find its messages disappearing into the runtime,
// which is why the set is small, fixed, and documented.
func (r *Runtime) ours(k schema.Kind) bool {
	switch k {
	case schema.KindJoin, schema.KindCommit, schema.KindSettle:
		return true
	}
	return false
}

// handleOurs takes one of the runtime's own messages.
func (r *Runtime) handleOurs(ctx context.Context, msg Message) error {
	switch msg.Kind {
	case schema.KindJoin:
		var j membership.Join
		if err := json.Unmarshal(msg.Body, &j); err != nil {
			return fmt.Errorf("read a join: %w", err)
		}
		if err := r.addJoin(msg.Match, &j); err != nil {
			return err
		}
		return r.seatIfReady(ctx, msg.Match)

	case schema.KindCommit:
		var c membership.Commit
		if err := json.Unmarshal(msg.Body, &c); err != nil {
			return fmt.Errorf("read a commit: %w", err)
		}
		if err := r.addCommit(msg.Match, &c); err != nil {
			return err
		}
		return r.seatIfReady(ctx, msg.Match)

	case schema.KindSettle:
		var st schema.Settle
		if err := json.Unmarshal(msg.Body, &st); err != nil {
			return fmt.Errorf("read a payout: %w", err)
		}
		return r.adoptSettlement(ctx, msg.Match, st)
	}
	return nil
}

// Run owns the loop until the context ends.
//
// Two things run: the bridge's control requests, and the frames addressed to
// this game. Both stop when the context does, and Run returns the first error
// that is not simply the context ending.
func (r *Runtime) Run(ctx context.Context) error {
	r.runCtx = ctx
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
	transport.Receive(ctx, frames, r.router)
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
