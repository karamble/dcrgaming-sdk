package transport

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/wire"
)

const (
	testSID   = "0123456789abcdef"
	testGCID  = "aa"
	testMatch = "table1|sess1"
	alice     = "1111111111111111111111111111111111111111111111111111111111111111"
	stranger  = "9999999999999999999999999999999999999999999999999999999999999999"
)

// fakeSender records what a router sent.
type fakeSender struct {
	mu    sync.Mutex
	sent  []string
	fail  error
	gcIDs []string
}

func (f *fakeSender) SendGC(_ context.Context, gcID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.gcIDs = append(f.gcIDs, gcID)
	f.sent = append(f.sent, text)
	return nil
}

func (f *fakeSender) frames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func newRouter(t *testing.T, send *fakeSender, allow func(sid, sender string) bool, got *[]Delivery) *Router {
	t.Helper()
	var mu sync.Mutex
	r, err := NewRouter(Config{
		Game:      "poker",
		GameVer:   5,
		Sender:    send,
		Authorize: allow,
		Handle: func(d Delivery) {
			mu.Lock()
			*got = append(*got, d)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	return r
}

func allowAll(string, string) bool { return true }

// A message sent through the router comes back out of it decoded, with the
// sender the transport authenticated rather than anything from the payload.
func TestRoundTripThroughTheRouter(t *testing.T) {
	send := &fakeSender{}
	var got []Delivery
	r := newRouter(t, send, allowAll, &got)

	err := r.Send(context.Background(), testGCID, testSID, testMatch,
		schema.KindHead, schema.Head{Seq: 4, Seat: 1}, wire.ClassTurn)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	frames := send.frames()
	if len(frames) != 1 {
		t.Fatalf("sent %d frames, want 1", len(frames))
	}

	r.HandleGCMessage(testGCID, alice, frames[0], time.Now())
	if len(got) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(got))
	}
	d := got[0]
	if d.Sender != alice || d.SID != testSID || d.GCID != testGCID {
		t.Fatalf("delivery lost its provenance: %+v", d)
	}
	if d.Msg.Kind != schema.KindHead || d.Msg.Match != testMatch {
		t.Fatalf("message header did not survive: %+v", d.Msg)
	}
	var head schema.Head
	if err := d.Msg.Into(&head); err != nil {
		t.Fatalf("into: %v", err)
	}
	if head.Seq != 4 || head.Seat != 1 {
		t.Fatalf("body did not survive: %+v", head)
	}
}

// Frames are invisible to the user, so buffering for a sender nobody vouched
// for would be a way to exhaust memory with nothing showing on screen.
func TestUnauthorizedSenderAllocatesNothing(t *testing.T) {
	send := &fakeSender{}
	var got []Delivery
	onlyAlice := func(_, sender string) bool { return sender == alice }
	r := newRouter(t, send, onlyAlice, &got)

	// A chunked message, so a part would create reassembly state if it were
	// admitted at all.
	parts, err := wire.Encode("poker", 5, testSID,
		[]byte(`{"v":1,"kind":"action","match":"t","body":{}}`), time.Time{}, 8)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("expected a chunked message, got %d parts", len(parts))
	}

	for _, p := range parts {
		r.HandleGCMessage(testGCID, stranger, p, time.Now())
	}
	if r.Pending() != 0 {
		t.Fatalf("an unauthorized sender allocated %d partial messages", r.Pending())
	}
	if len(got) != 0 {
		t.Fatal("an unauthorized sender was delivered")
	}
}

// Ordinary conversation and another game's traffic both pass through untouched.
func TestNonFramesAndOtherGamesAreIgnored(t *testing.T) {
	send := &fakeSender{}
	var got []Delivery
	r := newRouter(t, send, allowAll, &got)

	for _, text := range []string{
		"hello everyone",
		"",
		`--mcp[v=1,sid=ab,mid=cd,seq=1/1,exp=0]--QUJD`,
		`--gaming[v=1,game=chess,gv=1,sid=ab,mid=cd,seq=1/1,exp=0]--QUJD`,
	} {
		r.HandleGCMessage(testGCID, alice, text, time.Now())
	}
	if len(got) != 0 {
		t.Fatalf("delivered %d messages that were not ours", len(got))
	}
	if r.Pending() != 0 {
		t.Fatal("non-frames allocated reassembly state")
	}
}

// Group chat gives no ordering, so out-of-order chunks are the normal case.
func TestChunkedMessageReassemblesOutOfOrder(t *testing.T) {
	send := &fakeSender{}
	var got []Delivery
	r := newRouter(t, send, allowAll, &got)

	body := struct {
		After uint64 `json:"after"`
	}{After: 7}
	payload, err := schema.Encode(5, schema.Kind("app.test"), testMatch, body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	parts, err := wire.Encode("poker", 5, testSID, payload, time.Time{}, 16)
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	if len(parts) < 3 {
		t.Fatalf("expected several parts, got %d", len(parts))
	}

	for i := len(parts) - 1; i >= 0; i-- { // reversed
		r.HandleGCMessage(testGCID, alice, parts[i], time.Now())
	}
	if len(got) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(got))
	}
	var back struct {
		After uint64 `json:"after"`
	}
	if err := got[0].Msg.Into(&back); err != nil {
		t.Fatalf("into: %v", err)
	}
	if back.After != 7 {
		t.Fatalf("body did not survive chunking: %+v", back)
	}
}

// Two senders chunking at once must not have their parts merged. Under group
// chat fan-out every member sends separately, so this is the normal case.
func TestChunksFromDifferentSendersDoNotMerge(t *testing.T) {
	send := &fakeSender{}
	var got []Delivery
	r := newRouter(t, send, allowAll, &got)

	payload, _ := schema.Encode(5, schema.Kind("app.test"), testMatch, map[string]uint64{"after": 1})
	parts, _ := wire.Encode("poker", 5, testSID, payload, time.Time{}, 16)

	// Interleave one sender's first part with another's whole message.
	r.HandleGCMessage(testGCID, stranger, parts[0], time.Now())
	for _, p := range parts {
		r.HandleGCMessage(testGCID, alice, p, time.Now())
	}
	if len(got) != 1 {
		t.Fatalf("delivered %d messages, want alice's only", len(got))
	}
	if got[0].Sender != alice {
		t.Fatalf("delivery attributed to %s", got[0].Sender)
	}
}

// A router with no Authorize allows nobody: defaulting the other way would turn
// a forgotten wiring step into an open door.
func TestMissingAuthorizeAllowsNobody(t *testing.T) {
	send := &fakeSender{}
	var got []Delivery
	r, err := NewRouter(Config{
		Game: "poker", GameVer: 5, Sender: send,
		Handle: func(d Delivery) { got = append(got, d) },
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	payload, _ := schema.Encode(5, schema.Kind("app.test"), testMatch, map[string]uint64{"after": 1})
	parts, _ := wire.Encode("poker", 5, testSID, payload, time.Time{}, 0)

	r.HandleGCMessage(testGCID, alice, parts[0], time.Now())
	if len(got) != 0 {
		t.Fatal("a router with no authorization delivered a message")
	}
}

func TestNewRouterRefusesIncompleteConfig(t *testing.T) {
	send := &fakeSender{}
	for name, cfg := range map[string]Config{
		"no game":         {GameVer: 5, Sender: send, Handle: func(Delivery) {}},
		"no game version": {Game: "poker", Sender: send, Handle: func(Delivery) {}},
		"no sender":       {Game: "poker", GameVer: 5, Handle: func(Delivery) {}},
		"no handle":       {Game: "poker", GameVer: 5, Sender: send},
	} {
		if _, err := NewRouter(cfg); err == nil {
			t.Errorf("%s should be refused", name)
		}
	}
}

// The publisher's version guard lives in Send as well as the constructor,
// because the fields are exported and a bare literal skips NewPublisher.
func TestPublisherRefusesAMissingGameVersion(t *testing.T) {
	send := &fakeSender{}
	if _, err := NewPublisher("poker", 0, send); err == nil {
		t.Fatal("a publisher with no game version should be refused")
	}
	p := &Publisher{Game: "poker", Sender: send}
	err := p.Send(context.Background(), testGCID, testSID, testMatch,
		schema.KindHead, schema.Head{Seq: 1}, wire.ClassTurn)
	if err == nil {
		t.Fatal("a bare publisher with no game version sent anyway")
	}
	if len(send.frames()) != 0 {
		t.Fatal("frames went out despite the refusal")
	}
}

// Receive is the loop a game runs; it must stop when told to.
//
// The delivery is waited for on a channel rather than by watching a slice
// grow. Every other test here drives the router on the test's own goroutine and
// can read what came out directly; this one is the only place a delivery
// happens on another goroutine, and polling `len` across that boundary is a
// data race - one the detector reports, which means it fails CI.
func TestReceiveFeedsTheRouterAndStops(t *testing.T) {
	send := &fakeSender{}
	delivered := make(chan Delivery, 1)
	r, err := NewRouter(Config{
		Game:      "poker",
		GameVer:   5,
		Sender:    send,
		Authorize: allowAll,
		Handle:    func(d Delivery) { delivered <- d },
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	payload, _ := schema.Encode(5, schema.Kind("app.test"), testMatch, map[string]uint64{"after": 3})
	parts, _ := wire.Encode("poker", 5, testSID, payload, time.Time{}, 0)

	frames := make(chan InboundFrame, 1)
	frames <- InboundFrame{Game: "poker", GCID: testGCID, From: alice, Frame: parts[0]}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Receive(ctx, frames, r); close(done) }()

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("frame never reached the router")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Receive did not stop when cancelled")
	}
}
