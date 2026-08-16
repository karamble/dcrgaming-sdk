package transport

import (
	"context"
	"fmt"
	"time"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
)

// Everything the game asks the bridge for, and the one stream it listens on.
//
// The shapes here are the game's own - ChainTip, Outpoint, Spend - and the wire
// types stay behind this file. That is not ceremony: the game had these types
// before there was a bridge, and the parts of it that reason about money should
// not have to change because a transport did.

// SendGC carries a frame out to a table's group chat.
//
// The bridge checks only that the frame is this game's own; what it means is
// between the players, who sign their own traffic and check each other's.
func (c *Bridge) SendGC(ctx context.Context, gcID, text string) error {
	_, err := c.rpc.SendFrame(ctx, &gamingpb.SendFrameRequest{Gcid: gcID, Frame: text})
	if err != nil {
		return hostErr("send a frame", err)
	}
	return nil
}

// ChainTip reports the best block the bridge's node knows.
func (c *Bridge) ChainTip(ctx context.Context) (ChainTip, error) {
	reply, err := c.rpc.ChainTip(ctx, &gamingpb.ChainTipRequest{})
	if err != nil {
		return ChainTip{}, hostErr("read the chain tip", err)
	}
	return ChainTip{Height: reply.GetHeight(), Hash: reply.GetHash()}, nil
}

// BlockHash reports the hash of the block at a height.
func (c *Bridge) BlockHash(ctx context.Context, height uint32) (string, error) {
	reply, err := c.rpc.BlockHash(ctx, &gamingpb.BlockHashRequest{Height: height})
	if err != nil {
		return "", hostErr(fmt.Sprintf("read the hash of block %d", height), err)
	}
	return reply.GetHash(), nil
}

// Outpoint reports what the chain says about one confirmed output.
func (c *Bridge) Outpoint(ctx context.Context, txid string, vout uint32) (Outpoint, error) {
	return c.outpoint(ctx, txid, vout, false)
}

// UnconfirmedOutpoint is the same question including the mempool.
//
// Separate from Outpoint on purpose, and the two are not interchangeable:
// anything that decides something waits for confirmations, and only our own
// just-broadcast money is looked at unconfirmed.
func (c *Bridge) UnconfirmedOutpoint(ctx context.Context, txid string, vout uint32) (Outpoint, error) {
	return c.outpoint(ctx, txid, vout, true)
}

func (c *Bridge) outpoint(ctx context.Context, txid string, vout uint32, mempool bool) (Outpoint, error) {
	reply, err := c.rpc.Outpoint(ctx, &gamingpb.OutpointRequest{
		Txid: txid, Vout: vout, IncludeMempool: mempool,
	})
	if err != nil {
		return Outpoint{}, hostErr(fmt.Sprintf("look up %s:%d", txid, vout), err)
	}
	return Outpoint{
		Found:         reply.GetFound(),
		ValueAtoms:    reply.GetValueAtoms(),
		PkScriptHex:   reply.GetPkScriptHex(),
		Confirmations: reply.GetConfirmations(),
		Coinbase:      reply.GetCoinbase(),
	}, nil
}

// Broadcast relays a transaction this game signed itself.
//
// The bridge bounds it by shape rather than by intent: it refuses anything
// taking coin that is not this game's, and anything moving coin the game could
// move on its own to somewhere it did not come from.
func (c *Bridge) Broadcast(ctx context.Context, rawTxHex string) (string, error) {
	reply, err := c.rpc.Broadcast(ctx, &gamingpb.BroadcastRequest{RawTxHex: rawTxHex})
	if err != nil {
		return "", hostErr("relay a transaction", err)
	}
	return reply.GetTxid(), nil
}

// RequestSpend asks a person to pay an address, and returns as soon as the
// request is recorded - not when it is paid.
//
// The reason travels because a person is going to read it. "Fund a bond" and
// "buy into a table" are the same amount to a cap and entirely different things
// to somebody deciding.
func (c *Bridge) RequestSpend(ctx context.Context, address string, amountAtoms int64, reason string) (Spend, error) {
	reply, err := c.rpc.RequestSpend(ctx, &gamingpb.RequestSpendRequest{
		Address: address, AmountAtoms: amountAtoms, Reason: reason,
	})
	if err != nil {
		return Spend{}, hostErr("ask for a payment", err)
	}
	return spendFrom(reply), nil
}

// SpendStatus reports what became of one of this game's requests.
func (c *Bridge) SpendStatus(ctx context.Context, id string) (Spend, error) {
	reply, err := c.rpc.SpendStatus(ctx, &gamingpb.SpendStatusRequest{Id: id})
	if err != nil {
		return Spend{}, hostErr("ask about a payment", err)
	}
	return spendFrom(reply), nil
}

// AwaitSpend waits for a person to decide, and reports what they decided.
//
// Polling is the answer of record even though the stream carries a hint that a
// request settled: the hint can be missed, and a game that trusted it would
// wait forever on the one that was.
func (c *Bridge) AwaitSpend(ctx context.Context, id string) (Spend, error) {
	ticker := time.NewTicker(spendPoll)
	defer ticker.Stop()

	for {
		spend, err := c.SpendStatus(ctx, id)
		if err != nil {
			return Spend{}, err
		}
		if spend.Settled() {
			return spend, nil
		}
		select {
		case <-ctx.Done():
			return Spend{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func spendFrom(s *gamingpb.Spend) Spend {
	return Spend{
		ID:          s.GetId(),
		Game:        s.GetGame(),
		Address:     s.GetAddress(),
		AmountAtoms: s.GetAmountAtoms(),
		Reason:      s.GetReason(),
		State:       SpendState(s.GetState()),
		TxID:        s.GetTxid(),
		Error:       s.GetError(),
		RequestedAt: s.GetRequestedAt(),
		DecidedAt:   s.GetDecidedAt(),
		ExpiresAt:   s.GetExpiresAt(),
	}
}

// ReportState tells the bridge what this game is doing, so the console can show
// it. The bridge keeps the last one, which is what an operator sees while the
// game is not running.
func (c *Bridge) ReportState(ctx context.Context, state *gamingpb.GameState) error {
	if _, err := c.rpc.ReportState(ctx, state); err != nil {
		return hostErr("report state", err)
	}
	return nil
}

// Respond answers something the operator asked for.
//
// Every request gets one, including the ones that failed: the console is
// showing somebody a spinner, and an error they can read beats a button that
// never comes back.
func (c *Bridge) Respond(ctx context.Context, req *gamingpb.RespondRequest) error {
	if _, err := c.rpc.Respond(ctx, req); err != nil {
		return hostErr("answer a request", err)
	}
	return nil
}
