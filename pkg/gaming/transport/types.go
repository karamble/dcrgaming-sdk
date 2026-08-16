package transport

import (
	"context"
	"time"
)

// What the game says about the chain, about money, and about a frame.
//
// These are the game's own words and they predate the bridge. The wire types
// live behind the transport, so the parts of this program that reason about
// money did not have to change when the transport did.

// InboundFrame is one frame the host delivered.
type InboundFrame struct {
	Game  string `json:"game"`
	GCID  string `json:"gcid"`
	From  string `json:"from"`
	Frame string `json:"frame"`
}

// Receive feeds frames from the host into a router until ctx is cancelled.
//
// It takes a stream of already-decoded frames rather than opening the
// connection itself, so the wire protocol between game and host - today a
// websocket, tomorrow whatever the plugin model settles on - stays out of the
// routing logic and out of its tests.
func Receive(ctx context.Context, frames <-chan InboundFrame, r *Router) {
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-frames:
			if !ok {
				return
			}
			// The host has already decided this frame is ours, but the
			// router checks again: it is the thing that knows which
			// game it serves, and a host that got it wrong should not
			// be able to inject another game's traffic.
			r.HandleGCMessage(f.GCID, f.From, f.Frame, time.Now())
		}
	}
}

// ChainTip is where the chain is now.
type ChainTip struct {
	Height int64  `json:"height"`
	Hash   string `json:"hash"`
}

// Outpoint is what the chain says about one transaction output.
//
// Found is false for an outpoint that never existed, one already spent, and one
// still unconfirmed alike. For the question a game asks - is there coin here
// that everyone can see - those are the same answer.
type Outpoint struct {
	Found         bool   `json:"found"`
	ValueAtoms    int64  `json:"valueAtoms"`
	PkScriptHex   string `json:"pkScriptHex"`
	Confirmations int64  `json:"confirmations"`
	Coinbase      bool   `json:"coinbase"`
}

// SpendState is what became of a request.
type SpendState string

const (
	SpendPending  SpendState = "pending"
	SpendApproved SpendState = "approved"
	SpendDenied   SpendState = "denied"
	SpendExpired  SpendState = "expired"
	SpendFailed   SpendState = "failed"
)

// Spend is a request to move money, and what was decided about it.
type Spend struct {
	ID          string     `json:"id"`
	Game        string     `json:"game"`
	Address     string     `json:"address"`
	AmountAtoms int64      `json:"amountAtoms"`
	Reason      string     `json:"reason,omitempty"`
	State       SpendState `json:"state"`
	TxID        string     `json:"txid,omitempty"`
	Error       string     `json:"error,omitempty"`
	RequestedAt int64      `json:"requestedAt"`
	DecidedAt   int64      `json:"decidedAt,omitempty"`
	ExpiresAt   int64      `json:"expiresAt"`
}

// Settled reports whether the request has stopped being able to change.
func (s Spend) Settled() bool { return s.State != SpendPending }

// spendPoll is how often the bridge is asked whether somebody has answered. A
// person is being waited on, so this is patient by design.
const spendPoll = 3 * time.Second
