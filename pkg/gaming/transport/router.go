// Package transport carries game protocol frames over Bison Relay group chats.
//
// It is the join between framing and a table. Inbound, it recognises frames,
// decides whether the sender is allowed to be talking to this table at all,
// reassembles them and hands up decoded messages. Outbound, it splits a message
// into frames and sends them.
//
// What it does not do is as important. A Bison Relay group message is fanned
// out by its sender as separate one-to-one messages, so there is no shared
// stream, no ordering and no guarantee two members received the same thing.
// This layer therefore never implies any of that: it reports what arrived and
// who sent it. Sequencing and agreement live in pkg/gamelog, which signs and
// chains entries precisely because arrival order cannot be trusted.
//
// Authorization is injected rather than decided here, and the distinction it
// must draw is the whole reason. Membership of the group chat is administered
// by one member, is never attested, can differ between members at the same
// generation, and changes merely by receiving a message. It cannot be what
// decides who may spend a table's memory or be believed about its history. The
// caller supplies a predicate backed by the escrow roster - the one membership
// list everyone whose money is at stake has agreed to on-chain.
package transport

import (
	"context"
	"fmt"
	"time"

	"github.com/decred/slog"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/wire"
)

// GCSender delivers one message body to a Bison Relay group chat.
type GCSender interface {
	SendGC(ctx context.Context, gcID, text string) error
}

// Delivery is one decoded message and where it came from.
//
// Sender is the Bison Relay uid the message was authenticated as. It is not
// taken from the payload, and no payload field could override it.
type Delivery struct {
	GCID   string
	Sender string
	SID    string
	Msg    *schema.Message
}

// Config configures a Router.
type Config struct {
	// Game is the routing key this router answers to. Frames for any other
	// game are ignored - not surfaced, not buffered, not dispatched.
	Game string

	// GameVer is the schema version stamped on outbound frames.
	GameVer int

	Sender GCSender

	// Authorize reports whether a sender may talk to a table. It is called
	// before any state is allocated for them. A nil Authorize allows
	// nobody, because defaulting the other way would turn a forgotten
	// wiring step into an open memory-exhaustion channel.
	Authorize func(sid, sender string) bool

	// Handle receives decoded messages. It is called from the caller's
	// receive path, so it should not block.
	Handle func(Delivery)

	Assembler wire.AssemblerConfig
	Log       slog.Logger
}

// Router routes game frames to and from group chats.
type Router struct {
	cfg Config
	asm *wire.Assembler
}

func NewRouter(cfg Config) (*Router, error) {
	if cfg.Game == "" {
		return nil, fmt.Errorf("router must name the game it routes")
	}
	if cfg.GameVer <= 0 {
		return nil, fmt.Errorf("router must name the game version it speaks")
	}
	if cfg.Sender == nil {
		return nil, fmt.Errorf("router has no way to send")
	}
	if cfg.Handle == nil {
		return nil, fmt.Errorf("router has nowhere to deliver")
	}
	return &Router{cfg: cfg, asm: wire.NewAssembler(cfg.Assembler)}, nil
}

// HandleGCMessage feeds one group chat message in.
//
// Anything that is not a frame for this game is dropped in silence: the caller
// hands over every message the group carries, and ordinary conversation must
// pass through untouched.
//
// The order is parse, authorize, then allocate. Frames are invisible to the
// user by design, so buffering for an unknown sender would be a way to exhaust
// memory with nothing appearing on screen. One cost is paid before the check -
// parsing base64-decodes the payload - but it is bounded by the message size
// and nothing is retained.
func (r *Router) HandleGCMessage(gcID, sender, text string, now time.Time) {
	part, ok := wire.Parse(text)
	if !ok {
		return
	}
	if part.Game != r.cfg.Game {
		// Another game's traffic, or one this build does not have. Not
		// ours to interpret, and not chat either.
		return
	}
	if r.cfg.Authorize == nil || !r.cfg.Authorize(part.SID, sender) {
		r.debugf("dropping frame from unauthorized sender %s for table %s", sender, part.SID)
		return
	}

	// Key reassembly by group and sender together. A group message arrives
	// as a separate message from each sender with no shared stream, so a
	// key naming only the group would let one member's chunks be merged
	// with another's.
	payload, err := r.asm.Add(gcID+"/"+sender, part, now)
	if err != nil {
		r.debugf("dropping frame from %s: %v", sender, err)
		return
	}
	if payload == nil {
		return // still waiting on chunks
	}

	msg, err := schema.Decode(r.cfg.GameVer, payload)
	if err != nil {
		r.debugf("undecodable message from %s: %v", sender, err)
		return
	}
	r.cfg.Handle(Delivery{GCID: gcID, Sender: sender, SID: part.SID, Msg: msg})
}

// Send frames a message and delivers it to a group chat.
//
// class decides how long the message stays valid. It is a real choice: turn
// traffic must go stale quickly so a raise cannot land after settlement, while
// a dispute notice has to outlive a player rebooting, since the relay holds
// messages for players who are offline and an offline player is exactly who a
// dispute concerns.
func (r *Router) Send(ctx context.Context, gcID, sid, matchID string, kind schema.Kind, body any, class wire.Class) error {
	p := &Publisher{Game: r.cfg.Game, GameVer: r.cfg.GameVer, Sender: r.cfg.Sender}
	return p.Send(ctx, gcID, sid, matchID, kind, body, class)
}

// SetLog makes the router report what it drops. A router that discards a frame
// in silence is indistinguishable from one that never received it.
func (r *Router) SetLog(log slog.Logger) { r.cfg.Log = log }

// Pending reports how many partial messages are held, for metrics and tests.
func (r *Router) Pending() int { return r.asm.Pending() }

func (r *Router) debugf(format string, args ...interface{}) {
	if r.cfg.Log != nil {
		r.cfg.Log.Debugf(format, args...)
	}
}
