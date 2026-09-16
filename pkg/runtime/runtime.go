package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/slog"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
	"github.com/karamble/dcrgaming-sdk/pkg/identity"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

// ErrNotYet marked a stage the runtime did not carry out yet.
//
// Nothing returns it: every stage of the lifecycle is built. It is kept only so
// the two tests that assert *nothing* reports a stage as missing have something
// to assert against, and so a stage added later has an obvious sentinel to
// reach for.
//
// Do not branch on it in a game. A branch on this never fires, which is worse
// than not having written it.
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
	// Tables is where tables are kept between runs. Optional, and a game
	// that leaves it out keeps its tables in memory only - which means a
	// restart forgets every stake it has not yet settled and every bond it
	// has not yet released.
	Tables TableStore
	// Log is optional.
	Log slog.Logger
}

// Runtime owns the table lifecycle: invite, seat, fund, settle, forfeit,
// reclaim.
//
// A game hands it [Rules] and calls [Game] methods; it never drives.
type Runtime struct {
	persistMu         sync.Mutex
	admissionMu       sync.Mutex
	fundingMu         sync.Mutex
	obligationLocks   map[string]*sync.Mutex
	faultMu           sync.Mutex
	storageErr        error
	lifeMu            sync.Mutex
	workers           sync.WaitGroup
	running, stopping bool
	admissionWorkers  map[string]bool
	faultCh           chan struct{}
	resumeReport      ResumeReport
	releases          []func() error

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
	sweepMu sync.Mutex

	// runCtx is the context Run was given, so a message the router delivers
	// carries the same lifetime as the loop that fetched it.
	runCtx context.Context

	// store is where tables are kept between runs. Optional.
	store TableStore

	mu     sync.Mutex
	tables map[string]*table
	// ended is every table that finished aborted, and why. A tombstone
	// rather than a table: the state is terminal and has to stay terminal
	// across a restart, because a commit arriving after everyone else gave
	// up would otherwise put this process back into a membership nobody is
	// bound to.
	ended map[string]string
	names map[string]string
}

// table is one table's runtime state. Seating fills it in; the stages read it.
type table struct {
	formMu    sync.Mutex
	formPtrMu sync.RWMutex
	match     string
	gcID      string
	form      *membership.Formation
	seats     map[uint32][]byte

	// funded is where each seat's stake landed, filled in by the funding
	// stage. payouts is where each seat asked to be paid, which every seat
	// announces because it goes into the settlement they all sign.
	funded  map[uint32]staked
	payouts map[uint32][]byte

	// settle gathers signatures on this table's payout.
	payoutID     string
	bridgePayout string

	// seatBond is this table's bridge-controlled admission deposit.
	seatBond  staked
	bridgeKey string

	// terms are this table's, kept because a per-table seat bond has to be
	// funded before there is a formation to ask.
	terms membership.Terms

	recoveryOnly   bool
	recoveryReason string
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
	r := &Runtime{admissionWorkers: map[string]bool{}, faultCh: make(chan struct{}),
		rules: cfg.Rules, bridge: cfg.Bridge, book: cfg.Book, log: log,
		identity: cfg.Identity, seatTags: cfg.SeatTags, params: cfg.Params,
		store:  cfg.Tables,
		tables: map[string]*table{},
		ended:  map[string]string{},
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
	if err := r.acquireOwnership(); err != nil {
		return nil, err
	}
	return r, nil
}

// deliver takes one decoded message: the runtime's own if it is one, the
// game's otherwise.
//
// The runtime does not look inside a game's body and does not retry: a
// redelivered move is a replayed move, so a game that wants one has to ask.
func (r *Runtime) deliver(d transport.Delivery) {
	r.lifeMu.Lock()
	ctx := r.runCtx
	r.lifeMu.Unlock()
	msg := Message{Match: d.SID, GCID: d.GCID, From: d.Sender}
	if d.Msg != nil {
		msg.Kind, msg.Body = d.Msg.Kind, d.Msg.Body
	}
	if r.ours(msg.Kind) {
		if err := r.handleOurs(ctx, msg); err != nil {
			r.log.Warnf("a %s message was not taken: %v", msg.Kind, err)
		}
		return
	}
	if err := r.rules.Handle(ctx, msg); err != nil {
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
	case schema.KindJoin, schema.KindCommit,
		KindFunded, KindBonded, KindPayout, KindRoster, KindResync, KindResyncReply:
		return true
	}
	return false
}

// handleOurs takes one of the runtime's own messages.
func (r *Runtime) handleOurs(ctx context.Context, msg Message) error {
	switch msg.Kind {
	case schema.KindJoin:
		var body schema.Join
		if err := json.Unmarshal(msg.Body, &body); err != nil {
			return fmt.Errorf("read a join: %w", err)
		}
		j, err := body.Into()
		if err != nil {
			return fmt.Errorf("read a join: %w", err)
		}
		return r.addJoin(ctx, msg.Match, j)

	case schema.KindCommit:
		var body schema.Commit
		if err := json.Unmarshal(msg.Body, &body); err != nil {
			return fmt.Errorf("read a commit: %w", err)
		}
		c, err := body.Into()
		if err != nil {
			return fmt.Errorf("read a commit: %w", err)
		}
		return r.addCommit(ctx, msg.Match, c)

	case KindResync:
		var ask schema.Resync
		if err := json.Unmarshal(msg.Body, &ask); err != nil {
			return fmt.Errorf("read a resync: %w", err)
		}
		return r.answerResync(ctx, msg.Match, ask)

	case KindResyncReply:
		var reply schema.ResyncReply
		if err := json.Unmarshal(msg.Body, &reply); err != nil {
			return fmt.Errorf("read a resync answer: %w", err)
		}
		return r.adoptResync(ctx, msg.Match, reply)

	case KindRoster:
		var ros schema.Roster
		if err := json.Unmarshal(msg.Body, &ros); err != nil {
			return fmt.Errorf("read a roster: %w", err)
		}
		return r.adoptRoster(ctx, msg.Match, ros)

	case KindFunded:
		var f schema.Funded
		if err := json.Unmarshal(msg.Body, &f); err != nil {
			return fmt.Errorf("read where a stake is: %w", err)
		}
		return r.adoptFunded(ctx, msg.Match, f)

	case KindPayout:
		var pay schema.Payout
		if err := json.Unmarshal(msg.Body, &pay); err != nil {
			return fmt.Errorf("read a payout: %w", err)
		}
		return r.adoptPayout(ctx, msg.Match, pay)

	}
	return nil
}

// Run owns the loop until the context ends.
//
// Two things run: the bridge's control requests, and the frames addressed to
// this game. Both stop when the context does, and Run returns the first error
// that is not simply the context ending.
func (r *Runtime) Run(ctx context.Context) error {
	r.lifeMu.Lock()
	if r.running || r.stopping {
		r.lifeMu.Unlock()
		return fmt.Errorf("runtime already started")
	}
	ctx, cancel := context.WithCancel(ctx)
	r.runCtx = ctx
	r.running = true
	r.lifeMu.Unlock()
	defer func() {
		r.lifeMu.Lock()
		r.stopping = true
		r.lifeMu.Unlock()
		cancel()
		r.workers.Wait()
		r.lifeMu.Lock()
		r.running = false
		r.releaseOwnership()
		r.lifeMu.Unlock()
	}()
	r.mu.Lock()
	pending := make([]*table, 0, len(r.tables))
	for _, t := range r.tables {
		pending = append(pending, t)
	}
	r.mu.Unlock()
	for _, t := range pending {
		r.startAdmission(t)
	}

	// The bridge says when it dropped frames, and a dropped formation
	// message is one nobody will send again. Registered before the stream
	// opens, because the first thing a reconnecting bridge reports is the
	// gap it just had.
	r.bridge.SetOnGap(func(gcids []string) {
		if len(gcids) > 0 {
			r.log.Warnf("the bridge missed frames for %d table(s); asking for what we are short of", len(gcids))
		} else {
			r.log.Warnf("the bridge missed frames and could not say which tables; asking for what we are short of")
		}
		r.Resync(ctx)
	})

	frames, err := r.bridge.Events(ctx)
	if err != nil {
		return fmt.Errorf("subscribe to the bridge: %w", err)
	}

	errs := make(chan error, 2)
	var consumers sync.WaitGroup
	consumers.Add(2)
	go func() { defer consumers.Done(); errs <- r.serveRequests(ctx) }()
	go func() { defer consumers.Done(); errs <- r.route(ctx, frames) }()

	var first error
	select {
	case first = <-errs:
	case <-r.faultCh:
		first = r.healthy()
	case <-ctx.Done():
		first = ctx.Err()
	}
	cancel()
	// Both request and frame consumers finish before the caller closes stores.
	consumers.Wait()
	if first == nil {
		first = ctx.Err()
	}
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

// BlockHash is what a past block hashed to.
//
// A game anchors its own moves to blocks so that what it says happened can be
// placed in time by anybody afterwards, and an anchor is only worth anything
// if the hash it names can be checked. Reading is all this allows: nothing
// here builds or broadcasts, and a game that could broadcast could pay
// somebody.
func (r *Runtime) BlockHash(ctx context.Context, height uint32) (string, error) {
	return r.bridge.BlockHash(ctx, height)
}

// Seat is which seat at a table is this peer's own.
//
// A game needs it for almost everything it does - whose turn it is, whose
// board is whose, who a move came from - and cannot work it out from Seats
// alone, which says who is at the table and not which one is looking.
func (r *Runtime) Seat(match string) (uint32, bool) {
	t, err := r.tableOf(match)
	if err != nil {
		return 0, false
	}
	return t.formation().OurSeat()
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

// Terms are one table's money terms, as agreed. Empty for a table this game is
// not at.
//
// A read surface: what a table costs, how long its money is locked, what a rung
// of its accusation chain pays. A game shows these to a person before they
// commit, and cannot be expected to have kept its own copy of what the runtime
// agreed on its behalf.
func (r *Runtime) Terms(match string) membership.Terms {
	t, err := r.rawTable(match)
	if err != nil {
		return membership.Terms{}
	}
	return t.terms
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

func (t *table) formation() *membership.Formation {
	t.formPtrMu.RLock()
	defer t.formPtrMu.RUnlock()
	return t.form
}
func (t *table) setFormation(f *membership.Formation) {
	t.formPtrMu.Lock()
	defer t.formPtrMu.Unlock()
	t.form = f
}

// PayoutFor is the wallet destination independently established by the bridge.
func (r *Runtime) PayoutFor(match string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t := r.tables[match]; t != nil {
		return t.bridgePayout
	}
	return ""
}
