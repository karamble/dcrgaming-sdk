package runtime

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

// prepareBridgeDeposit asks for a verified descriptor before asking for money.
// Its script must match the one independently derived from signed membership.
func (r *Runtime) prepareBridgeDeposit(ctx context.Context, want *spend.Record) error {
	r.mu.Lock()
	t := r.tables[want.Match]
	enabled := t != nil && t.bridgeKey != ""
	r.mu.Unlock()
	if !enabled {
		return fmt.Errorf("table has no bridge spending authority")
	}
	terms := t.terms
	identity, _, err := r.seatKeys(want.Match)
	if err != nil {
		return err
	}
	req := &gamingpb.PrepareDepositRequest{Sid: want.Match, Kind: want.Purpose, AmountAtoms: want.Atoms, IdentityKey: hex.EncodeToString(identity.PubKey().SerializeCompressed())}
	switch want.Purpose {
	case "seatbond":
		key, err := r.seatBondKeyFor(want.Match)
		if err != nil {
			return err
		}
		req.IdentityKey = hex.EncodeToString(key.PubKey().SerializeCompressed())
		req.LockBlocks = terms.BondLockBlocks
	case "stake":
		if t.formation() == nil {
			return fmt.Errorf("funding requires signed membership")
		}
		seats, ok := t.formation().FundingSeats()
		if !ok {
			return fmt.Errorf("financial membership unavailable")
		}
		for _, key := range seats {
			req.Members = append(req.Members, hex.EncodeToString(key))
		}
		req.LockBlocks = terms.CSVBlocks

	default:
		return fmt.Errorf("bridge verification for %s is not available", want.Purpose)
	}
	reply, err := r.bridge.PrepareDeposit(ctx, req)
	if err != nil {
		return err
	}
	if reply.GetId() == "" || reply.GetPkScript() != want.PkScript || reply.GetAddress() != want.Address || reply.GetRecoveryKey() != t.bridgeKey {
		return fmt.Errorf("bridge descriptor differs from the independently derived deposit")
	}
	want.DepositID = reply.GetId()
	return nil
}
