package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
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
	// PunishTag is the game's own domain tag for punishment-key
	// announcements. Frozen like SeatTags, and the game's to state.
	// Optional: a game with no forfeitable bonds needs none.
	PunishTag []byte
	// AccuseFeeAtoms is what one rung of an accusation chain pays, for a
	// table whose Terms do not state it.
	//
	// Terms win where they say anything, because a fee both seats agreed is
	// stronger than one they each read from their own build. This exists for
	// dcrpoker, whose live tables predate the term and whose digest cannot
	// move to add it. Zero on both is refused rather than defaulted: a chain
	// built at a fee nobody chose has an attrition bound that is a guess.
	AccuseFeeAtoms uint64
	// ReclaimFeeAtoms is what a reclaim, a sweep and a release pay. Zero
	// means DefaultReclaimFee.
	//
	// A fee is an economic choice, the same reason the ladder's is a
	// parameter: dcrpoker has always paid 20,000 and adopting somebody
	// else's number would silently change bytes that move its money.
	ReclaimFeeAtoms int64
	// BondScope says how many seat bonds a player posts: one backing every
	// table, or a fresh one for each. An economic choice, and the two
	// existing games made different ones - see [BondScope]. Zero value is
	// one per identity.
	BondScope BondScope
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
	rules      Rules
	bridge     *transport.Bridge
	book       *spend.Book
	identity   *identity.Identity
	seatTags   identity.SeatTags
	params     stdaddr.AddressParams
	punishTag  []byte
	accuseFee  uint64
	bondScope  BondScope
	reclaimFee int64
	router     *transport.Router
	log        slog.Logger

	// sweepMu guards what this process has broadcast a spend of. Its own
	// lock because it is consulted from the reclaim path while the table
	// lock is not held.
	sweepMu  sync.Mutex
	sweeping map[string]bool

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
	ended  map[string]string
	payout string
	names  map[string]string
}

// table is one table's runtime state. Seating fills it in; the stages read it.
type table struct {
	match string
	gcID  string
	form  *membership.Formation
	seats map[uint32][]byte

	// funded is where each seat's stake landed, filled in by the funding
	// stage. payouts is where each seat asked to be paid, which every seat
	// announces because it goes into the settlement they all sign.
	funded  map[uint32]staked
	payouts map[uint32][]byte

	// settle gathers signatures on this table's payout.
	settle *settlement

	// punishPubs is every seat's announced punishment key, and punish is
	// ours. forfeitBonds are derived once every seat has announced.
	punishPubs      map[uint32][]byte
	punish          *secp256k1.PrivateKey
	forfeitBonds    map[uint32]membership.ForfeitableBond
	forfeitFunded   map[uint32]staked
	tableBondFunded map[uint32]staked
	// releases are the table bonds going home cooperatively, by the seat
	// each one pays. Both of them: a release pays its owner and nobody
	// else, so the two seats' releases are different transactions and each
	// needs the other's signature.
	releases map[uint32]*release
	// ladders are the accusation chains this table has built, by the seat
	// each one accuses. Both of them: the one against the opponent is the
	// one this peer may run, and the one against itself is the one it must
	// co-sign so the opponent can run that.
	ladders map[uint32]*ladder

	// seatBond is the bond this seat's join binds to, for a game that posts
	// one per table. Empty for a game that posts one per identity, where
	// the deposit is the identity's and not the table's.
	seatBond staked

	// terms are this table's, kept because a per-table seat bond has to be
	// funded before there is a formation to ask.
	terms membership.Terms

	// saidAt is the height this table last repeated its announcements at,
	// so they go out once a block rather than once a poll.
	saidAt int64
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
	reclaimFee := cfg.ReclaimFeeAtoms
	if reclaimFee <= 0 {
		reclaimFee = DefaultReclaimFee
	}
	r := &Runtime{
		rules: cfg.Rules, bridge: cfg.Bridge, book: cfg.Book, log: log,
		identity: cfg.Identity, seatTags: cfg.SeatTags, params: cfg.Params,
		punishTag: cfg.PunishTag, accuseFee: cfg.AccuseFeeAtoms,
		bondScope:  cfg.BondScope,
		reclaimFee: reclaimFee,
		store:      cfg.Tables,
		tables:     map[string]*table{},
		ended:      map[string]string{},
		names:      map[string]string{},
		runCtx:     context.Background(),
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
	case schema.KindJoin, schema.KindCommit, schema.KindSettle, KindPunishKey, KindRelease, KindAccusation,
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

	case schema.KindSettle:
		var st schema.Settle
		if err := json.Unmarshal(msg.Body, &st); err != nil {
			return fmt.Errorf("read a payout: %w", err)
		}
		return r.adoptSettlement(ctx, msg.Match, st)

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

	case KindBonded:
		var b schema.Bonded
		if err := json.Unmarshal(msg.Body, &b); err != nil {
			return fmt.Errorf("read where a bond is: %w", err)
		}
		return r.adoptBonded(ctx, msg.Match, b)

	case KindPayout:
		var pay schema.Payout
		if err := json.Unmarshal(msg.Body, &pay); err != nil {
			return fmt.Errorf("read a payout: %w", err)
		}
		return r.adoptPayout(ctx, msg.Match, pay)

	case KindPunishKey:
		var n membership.PunishNote
		if err := json.Unmarshal(msg.Body, &n); err != nil {
			return fmt.Errorf("read a punishment-key announcement: %w", err)
		}
		return r.adoptPunishKey(ctx, msg.Match, n)

	case KindRelease:
		var rel schema.Release
		if err := json.Unmarshal(msg.Body, &rel); err != nil {
			return fmt.Errorf("read a release: %w", err)
		}
		return r.adoptRelease(ctx, msg.Match, rel)

	case KindAccusation:
		var a schema.Accusation
		if err := json.Unmarshal(msg.Body, &a); err != nil {
			return fmt.Errorf("read an accusation: %w", err)
		}
		return r.adoptAccusation(ctx, msg.Match, a)
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
	if t.form != nil {
		return t.form.Terms()
	}
	return t.terms
}

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
