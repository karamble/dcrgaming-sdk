package wire

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrExpired  = errors.New("message deadline passed")
	ErrOverflow = errors.New("reassembly buffer full")
	ErrTooLarge = errors.New("message exceeds size bounds")
)

// NewID returns a random 16 hex character identifier for a session or message.
func NewID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Class is what a message is for, which decides how long it stays valid.
//
// A single deadline for everything cannot work. Turn traffic must go stale
// quickly, so a raise held by store-and-forward cannot land after the hand has
// settled. A dispute notice must not: Bison Relay holds messages for players
// who are offline, and a player who is offline is exactly the one a dispute
// concerns. Expiring those on the same short clock would let the relay sit on a
// notice while an honest player boots, then have the receiver discard it as
// stale - quietly removing the escape hatch the dispute window exists to give
// them.
type Class uint8

const (
	// ClassTurn is an action or other in-hand traffic.
	ClassTurn Class = iota
	// ClassState is state sync, which is worth having late but not much later.
	ClassState
	// ClassDispute is evidence, and must outlive the dispute window.
	ClassDispute
	// ClassForm is formation traffic: joins, rosters, commits and the
	// resyncs that carry them. Its real staleness gate is not a clock at
	// all - a join is admitted or refused by where the chain stands against
	// the table's Until height, and a formation that closed no-ops every
	// late arrival - so the wire deadline only has to outlive a relay
	// backlog. Ten minutes did not: a join queued behind a peer's stale
	// backlog expired in transit, one table formed while the other aborted,
	// and no repeat could help because every repeat carried the same short
	// deadline. The residue of a long deadline is one ~30s race - a join
	// delivered after Until but before the tick that closes the window is
	// admitted - and the advance invariant already resolves that to no
	// game, never two tables.
	ClassForm
)

// TTL is how long a message of this class stays valid.
func (c Class) TTL() time.Duration {
	switch c {
	case ClassTurn:
		return 90 * time.Second
	case ClassState:
		return 10 * time.Minute
	case ClassDispute:
		// Comfortably beyond a dispute window sized to how long it takes
		// somebody to boot a computer and start Bison Relay.
		return 24 * time.Hour
	case ClassForm:
		// Longer than any backlog a live table has produced, and bounded
		// because an expiry is still the backstop against a relay
		// replaying ancient traffic at a fresh session id.
		return 24 * time.Hour
	}
	return 90 * time.Second
}

// Deadline is the expiry to stamp on a message of this class.
func (c Class) Deadline(now time.Time) time.Time { return now.Add(c.TTL()) }

// AssemblerConfig bounds reassembly.
type AssemblerConfig struct {
	MaxPendingPerSender int
	MaxMessageBytes     int
	MaxTotalBytes       int
	Timeout             time.Duration
}

func (c AssemblerConfig) withDefaults() AssemblerConfig {
	if c.MaxPendingPerSender <= 0 {
		c.MaxPendingPerSender = 8
	}
	if c.MaxMessageBytes <= 0 {
		c.MaxMessageBytes = 16 << 20
	}
	if c.MaxTotalBytes <= 0 {
		c.MaxTotalBytes = 64 << 20
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Minute
	}
	return c
}

// Assembler reassembles chunked messages.
//
// Frames are invisible to the user by design, so an unsolicited flood is a
// silent way to exhaust memory: nothing would appear on screen while the
// buffers filled. Callers must therefore authorize a sender before calling Add,
// and everything here is bounded so that even an authorized one cannot grow
// without limit.
//
// The sender key is opaque. Over a group chat it should identify both the group
// and the sender, because a group message is delivered as a separate message
// from each sender and there is no shared stream to key on.
type Assembler struct {
	cfg AssemblerConfig

	mu      sync.Mutex
	pending map[string]*pendingMsg
	bytes   int
}

type pendingMsg struct {
	sender    string
	total     int
	exp       int64
	firstSeen time.Time
	parts     map[int][]byte
	bytes     int
}

func NewAssembler(cfg AssemblerConfig) *Assembler {
	return &Assembler{cfg: cfg.withDefaults(), pending: make(map[string]*pendingMsg)}
}

// Add feeds one part in. It returns the complete payload once the last part of
// a message arrives, and nil while a message is still incomplete.
//
// A single-part message allocates nothing at all: it is returned before any
// lock is taken or any map touched, so the common case cannot contribute to
// exhaustion.
func (a *Assembler) Add(sender string, p *Part, now time.Time) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("no part")
	}
	if p.Expired(now) {
		return nil, ErrExpired
	}
	if p.Total == 1 {
		return p.Chunk, nil
	}

	key := sender + "/" + p.SID + "/" + p.MID

	a.mu.Lock()
	defer a.mu.Unlock()

	// Evict before admitting, so a stalled message cannot hold space
	// against a live one.
	a.gcLocked(now)

	msg, ok := a.pending[key]
	if !ok {
		if a.pendingForLocked(sender) >= a.cfg.MaxPendingPerSender {
			return nil, ErrOverflow
		}
		msg = &pendingMsg{
			sender:    sender,
			total:     p.Total,
			exp:       p.Exp,
			firstSeen: now,
			parts:     make(map[int][]byte, p.Total),
		}
		a.pending[key] = msg
	}
	if msg.total != p.Total {
		delete(a.pending, key)
		a.bytes -= msg.bytes
		return nil, fmt.Errorf("part claims %d of %d, message has %d", p.Seq, p.Total, msg.total)
	}
	// A repeated part is not new information and must not be charged for.
	if _, dup := msg.parts[p.Seq]; dup {
		return nil, nil
	}
	if msg.bytes+len(p.Chunk) > a.cfg.MaxMessageBytes || a.bytes+len(p.Chunk) > a.cfg.MaxTotalBytes {
		delete(a.pending, key)
		a.bytes -= msg.bytes
		return nil, ErrTooLarge
	}

	msg.parts[p.Seq] = p.Chunk
	msg.bytes += len(p.Chunk)
	a.bytes += len(p.Chunk)

	if len(msg.parts) < msg.total {
		return nil, nil
	}

	out := make([]byte, 0, msg.bytes)
	for i := 1; i <= msg.total; i++ {
		out = append(out, msg.parts[i]...)
	}
	delete(a.pending, key)
	a.bytes -= msg.bytes
	return out, nil
}

// Pending reports how many partial messages are held, for tests and metrics.
func (a *Assembler) Pending() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
}

func (a *Assembler) pendingForLocked(sender string) int {
	n := 0
	for _, msg := range a.pending {
		if msg.sender == sender {
			n++
		}
	}
	return n
}

// gcLocked drops partial messages that expired or went quiet.
//
// There is no negative acknowledgement and no retransmit: a message missing a
// part simply never completes and eventually evaporates. That is deliberate.
// The layer above already has to cope with a message that never arrives, since
// nothing about a group chat guarantees delivery, so adding a repair protocol
// here would duplicate recovery that has to exist anyway.
func (a *Assembler) gcLocked(now time.Time) {
	for key, msg := range a.pending {
		stale := msg.exp > 0 && now.Unix() > msg.exp
		if stale || now.Sub(msg.firstSeen) > a.cfg.Timeout {
			delete(a.pending, key)
			a.bytes -= msg.bytes
		}
	}
}
