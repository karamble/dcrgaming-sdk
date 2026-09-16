package transport

import (
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"testing"
)

func TestSubscriptionStatusOnlyFollowsStreamStart(t *testing.T) {
	c := &Bridge{}
	state, changed := c.ConnectionStatus()
	if state != Stopped {
		t.Fatal(state)
	}
	c.setStatus(Connecting)
	state, _ = c.ConnectionStatus()
	if state == Subscribed {
		t.Fatal("premature subscription")
	}
	c.handleStart(&gamingpb.StreamStart{Epoch: "epoch", FromSeq: 1})
	state, _ = c.ConnectionStatus()
	if state != Subscribed {
		t.Fatal(state)
	}
	// UI may ignore notices indefinitely without blocking the transport.
	for range 1000 {
		c.setStatus(Reconnecting)
	}
	select {
	case <-changed:
	default:
		t.Fatal("no state notification")
	}
	state, _ = c.ConnectionStatus()
	if state != Reconnecting {
		t.Fatal(state)
	}
}
