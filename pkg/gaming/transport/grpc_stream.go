package transport

import (
	"context"
	"time"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
)

// The one channel from the bridge to this game.
//
// Everything the operator decides arrives here, and so does every frame from
// this game's tables. There is exactly one, by design: two would need an order
// between them, and there is no useful answer to "did the invitation or the
// first frame of that table arrive first".

// Backoff between attempts to re-establish the stream. It never gives up: a
// game reaches its tables through the bridge and nothing else, so stopping
// leaves nothing working.
const (
	streamRetryMin = 1 * time.Second
	streamRetryMax = 30 * time.Second
)

// Events is the stream of frames addressed to this game.
//
// The channel is closed only when ctx is done, so a caller can range over it
// for the life of the program. Losing the connection is not the end of the
// stream; it is a pause in it.
func (c *Bridge) Events(ctx context.Context) (<-chan InboundFrame, error) {
	go c.streamForever(ctx)
	return c.frames, nil
}

// Requests is what the operator asked this game to do.
//
// Separate from the frames because they are answered rather than routed: every
// one carries a request id, and the console is waiting for a Respond against it.
func (c *Bridge) Requests() <-chan *gamingpb.BridgeRequest { return c.requests }

func (c *Bridge) streamForever(ctx context.Context) {
	defer close(c.frames)

	wait := streamRetryMin
	for {
		err := c.streamOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.warnf("the bridge's stream ended (%v); trying again in %s", err, wait)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait *= 2; wait > streamRetryMax {
			wait = streamRetryMax
		}
	}
}

// streamOnce holds one stream until it breaks.
func (c *Bridge) streamOnce(ctx context.Context) error {
	c.mu.Lock()
	epoch, lastSeq := c.epoch, c.lastSeq
	c.mu.Unlock()

	// Sent back so the bridge can tell a reconnect that missed nothing from
	// one that did. Guessing either way costs something: claiming to be up to
	// date loses frames silently, and claiming otherwise resynchronises every
	// table on every failed dial.
	stream, err := c.rpc.Subscribe(ctx, &gamingpb.SubscribeRequest{
		Epoch: epoch, LastSeq: lastSeq,
	})
	if err != nil {
		return hostErr("open the bridge's stream", err)
	}

	for {
		ev, err := stream.Recv()
		if err != nil {
			return hostErr("read from the bridge's stream", err)
		}

		switch e := ev.GetEvent().(type) {
		case *gamingpb.BridgeEvent_Start:
			c.handleStart(e.Start)
		case *gamingpb.BridgeEvent_Frame:
			c.handleFrame(ctx, e.Frame)
		case *gamingpb.BridgeEvent_Request:
			c.handleRequest(e.Request)
		case *gamingpb.BridgeEvent_Spend:
			// A hint that a request settled, so a poll can wake early.
			// Never the answer: whoever is waiting asks for themselves.
			c.debugf("the bridge says payment %s settled", e.Spend.GetId())
		}
	}
}

// handleStart records where the stream begins, and says so if frames were lost.
func (c *Bridge) handleStart(start *gamingpb.StreamStart) {
	c.mu.Lock()
	c.epoch, c.lastSeq = start.GetEpoch(), start.GetFromSeq()
	c.mu.Unlock()

	if !start.GetGap() {
		c.debugf("stream open at %d, nothing missed", start.GetFromSeq())
		return
	}
	// Only here. The bridge is the only thing that knows whether anything
	// was actually lost, and this is the only place it says so.
	c.warnf("the bridge missed frames for this game (%s, %d table(s) named)",
		start.GetGapScope(), len(start.GetGapGcids()))
	if c.cfg.OnGap != nil {
		c.cfg.OnGap(start.GetGapGcids())
	}
}

// handleFrame passes one frame on, and remembers how far the stream got.
func (c *Bridge) handleFrame(ctx context.Context, f *gamingpb.Frame) {
	c.mu.Lock()
	c.lastSeq = f.GetSeq()
	c.mu.Unlock()

	// Blocking here is deliberate. The router on the other end is what turns
	// a frame into a table's state, and dropping one silently would be a
	// table that disagrees with its peers about what happened.
	select {
	case c.frames <- InboundFrame{
		Game:  c.game,
		GCID:  f.GetGcid(),
		From:  f.GetFrom(),
		Frame: f.GetFrame(),
	}:
	case <-ctx.Done():
	}
}

// handleRequest queues something the operator asked for.
//
// Dropped rather than waited on if nothing is draining: these are answered by
// the game's own handlers, and one that has wedged must not also stop the
// frames arriving on the same stream.
func (c *Bridge) handleRequest(req *gamingpb.BridgeRequest) {
	select {
	case c.requests <- req:
	default:
		c.warnf("dropping request %s: this game is not answering them", req.GetRequestId())
	}
}
