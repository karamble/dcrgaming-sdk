package transport

import (
	"context"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
)

func (c *Bridge) FinancialKey(ctx context.Context, sid string) (string, error) {
	reply, err := c.rpc.FinancialKey(ctx, &gamingpb.FinancialKeyRequest{Sid: sid})
	if err != nil {
		return "", hostErr("get bridge spending authority", err)
	}
	return reply.GetPublicKey(), nil
}
func (c *Bridge) PrepareDeposit(ctx context.Context, req *gamingpb.PrepareDepositRequest) (*gamingpb.PreparedDeposit, error) {
	reply, err := c.rpc.PrepareDeposit(ctx, req)
	if err != nil {
		return nil, hostErr("verify deposit with bridge", err)
	}
	return reply, nil
}
func (c *Bridge) RequestDepositSpend(ctx context.Context, id, address string, atoms int64, reason string) (Spend, error) {
	reply, err := c.rpc.RequestSpend(ctx, &gamingpb.RequestSpendRequest{DepositId: id, Address: address, AmountAtoms: atoms, Reason: reason})
	if err != nil {
		return Spend{}, hostErr("request verified deposit funding", err)
	}
	return spendFrom(reply), nil
}

// ProposePayout never signs; it asks the bridge to present an exact approval.
func (c *Bridge) ProposePayout(ctx context.Context, req *gamingpb.ProposePayoutRequest) (*gamingpb.PayoutStatusReply, error) {
	out, err := c.rpc.ProposePayout(ctx, req)
	if err != nil {
		return nil, hostErr("propose payout", err)
	}
	return out, nil
}
func (c *Bridge) PayoutStatus(ctx context.Context, id string) (*gamingpb.PayoutStatusReply, error) {
	out, err := c.rpc.PayoutStatus(ctx, &gamingpb.PayoutStatusRequest{Id: id})
	if err != nil {
		return nil, hostErr("read payout status", err)
	}
	return out, nil
}

func (c *Bridge) FinancialAuthority(ctx context.Context, sid string) (*gamingpb.FinancialKeyReply, error) {
	reply, err := c.rpc.FinancialKey(ctx, &gamingpb.FinancialKeyRequest{Sid: sid})
	if err != nil {
		return nil, hostErr("read bridge financial authority", err)
	}
	return reply, nil
}

func (c *Bridge) FinancialState(ctx context.Context, sid string) (*gamingpb.FinancialStateReply, error) {
	reply, err := c.rpc.FinancialState(ctx, &gamingpb.FinancialStateRequest{Sid: sid})
	if err != nil {
		return nil, hostErr("read financial table state", err)
	}
	return reply, nil
}
