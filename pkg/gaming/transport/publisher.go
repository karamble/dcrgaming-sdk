package transport

import (
	"context"
	"fmt"
	"time"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/wire"
)

// Publisher frames messages and sends them to a group chat.
//
// It is the sending half on its own, for a participant that only broadcasts.
// A server relaying signed actions is one: it has no seat and no key, so there
// is nothing addressed to it and nothing it could contribute beyond carrying
// what the seats signed.
type Publisher struct {
	Game    string
	GameVer int
	Sender  GCSender
}

// NewPublisher returns a Publisher.
func NewPublisher(game string, gameVer int, sender GCSender) (*Publisher, error) {
	if game == "" {
		return nil, fmt.Errorf("publisher must name the game it sends as")
	}
	if gameVer <= 0 {
		return nil, fmt.Errorf("publisher must name the game version it sends as")
	}
	if sender == nil {
		return nil, fmt.Errorf("publisher has no way to send")
	}
	return &Publisher{Game: game, GameVer: gameVer, Sender: sender}, nil
}

// Send frames a message and delivers it.
//
// class decides how long the message stays valid, and it is a real choice: turn
// traffic must go stale quickly so a raise cannot land after settlement, while
// a dispute notice has to outlive a player rebooting - the relay holds messages
// for players who are offline, and an offline player is exactly who a dispute
// concerns.
func (p *Publisher) Send(ctx context.Context, gcID, sid, matchID string, kind schema.Kind, body any, class wire.Class) error {
	// Checked here as well as in NewPublisher: the fields are exported, so a
	// bare literal never went through the constructor.
	if p.GameVer <= 0 {
		return fmt.Errorf("publisher has no game version to stamp")
	}
	payload, err := schema.Encode(p.GameVer, kind, matchID, body)
	if err != nil {
		return err
	}
	parts, err := wire.Encode(p.Game, p.GameVer, sid, payload, class.Deadline(time.Now()), 0)
	if err != nil {
		return fmt.Errorf("frame %s: %w", kind, err)
	}
	for i, text := range parts {
		if err := p.Sender.SendGC(ctx, gcID, text); err != nil {
			// Partial delivery is not recoverable here: the receiver
			// will never complete the message and will evict it. Say
			// which part failed so the caller can retry the whole
			// message rather than guess.
			return fmt.Errorf("send %s part %d of %d: %w", kind, i+1, len(parts), err)
		}
	}
	return nil
}
