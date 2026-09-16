package runtime

import (
	"context"
	"encoding/hex"
	"fmt"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/wire"
	"github.com/karamble/dcrgaming-sdk/pkg/finance"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"sort"
)

// Settle submits a locally verified outcome for dashboard approval. The SDK
// neither holds financial keys nor signs, assembles or broadcasts payouts.
func (r *Runtime) Settle(ctx context.Context, match string, out Outcome) error {
	r.mu.Lock()
	t := r.tables[match]
	r.mu.Unlock()
	if t == nil || t.formation() == nil {
		return fmt.Errorf("unknown table")
	}
	seats, ok := t.formation().Seats()
	if !ok {
		return fmt.Errorf("table is not seated")
	}
	funds, ok := t.formation().FundingSeats()
	if !ok {
		return fmt.Errorf("financial roster unavailable")
	}
	order := seatOrder(seats)
	for _, seat := range order {
		if !r.willCoSign(match, seat) {
			return fmt.Errorf("game has not verified this payout")
		}
	}
	r.mu.Lock()
	shares, err := r.sharesFor(t, order, out)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	req := &gamingpb.ProposePayoutRequest{Sid: match}
	for _, seat := range order {
		staked := t.funded[seat]
		prev, err := outpointOf(staked.outpoint)
		if err != nil {
			r.mu.Unlock()
			return err
		}
		owner := hex.EncodeToString(funds[seat])
		req.Inputs = append(req.Inputs, &gamingpb.PayoutInput{OwnerKey: owner, IdentityKey: hex.EncodeToString(seats[seat]), Txid: prev.Hash.String(), Vout: prev.Index})
		if amount := shares[seat]; amount > 0 {
			req.Payments = append(req.Payments, &gamingpb.PayoutPayment{OwnerKey: owner, AmountAtoms: amount})
		}
	}
	r.mu.Unlock()
	status, err := r.bridge.ProposePayout(ctx, req)
	if err != nil {
		return err
	}
	r.mu.Lock()
	t.payoutID = status.GetId()
	r.mu.Unlock()
	return r.keep(t)
}

func (r *Runtime) proposeSettlement(ctx context.Context, t *table) error {
	r.mu.Lock()
	id := t.payoutID
	r.mu.Unlock()
	if id == "" {
		return nil
	}
	status, err := r.bridge.PayoutStatus(ctx, id)
	if err != nil {
		return err
	}
	if status.GetState() == "confirmed" {
		if hook, ok := r.rules.(Settled); ok {
			hook.Settled(ctx, t.match, status.GetTxid())
		}
	}
	return nil
}

// seatOrder is the order a settlement's inputs and outputs are built in.
//
// Ascending by seat, and it has to be: the order decides the transaction's
// bytes, and two honest peers iterating a Go map would each build a different
// transaction and produce signatures that cannot be combined.
func seatOrder(seats map[uint32][]byte) []uint32 {
	order := make([]uint32, 0, len(seats))
	for seat := range seats {
		order = append(order, seat)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	return order
}

// sharesFor works out what each seat is paid, and refuses an outcome that does
// not add up to what the table holds.
//
// A void table is not an even split and not a refusal: every seat takes its own
// stake back, which is the one division nobody can dispute.
func (r *Runtime) sharesFor(t *table, order []uint32, out Outcome) (map[uint32]int64, error) {
	held := int64(0)
	for _, seat := range order {
		amount := t.funded[seat].atoms
		if amount <= 0 || amount > finance.MaxAtoms-held {
			return nil, fmt.Errorf("invalid funded amount")
		}
		held += amount
	}
	if out.Void {
		shares := make(map[uint32]int64, len(order))
		for _, seat := range order {
			shares[seat] = t.funded[seat].atoms
		}
		return shares, nil
	}

	var paid int64
	for seat, amount := range out.Shares {
		if !seatedIn(order, seat) {
			return nil, fmt.Errorf("the outcome pays seat %d, which is not at this table", seat)
		}
		if amount < 0 || amount > finance.MaxAtoms-paid {
			return nil, fmt.Errorf("the outcome pays seat %d a negative %d", seat, amount)
		}
		paid += amount
	}
	if paid != held {
		return nil, fmt.Errorf("the outcome divides %d atoms and the table holds %d; "+
			"a settlement that does not add up is one the other seats refuse", paid, held)
	}
	shares := make(map[uint32]int64, len(order))
	for _, seat := range order {
		shares[seat] = out.Shares[seat]
	}
	return shares, nil
}

func seatedIn(order []uint32, seat uint32) bool {
	for _, s := range order {
		if s == seat {
			return true
		}
	}
	return false
}

// outpointOf turns "txid:vout" into a prevout.
func outpointOf(s string) (wire.OutPoint, error) {
	txid, vout, err := splitOutpoint(s)
	if err != nil {
		return wire.OutPoint{}, err
	}
	h, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return wire.OutPoint{}, fmt.Errorf("outpoint txid: %w", err)
	}
	return wire.OutPoint{Hash: *h, Index: vout, Tree: wire.TxTreeRegular}, nil
}

func seatedKey(seats map[uint32][]byte, key []byte) bool {
	for _, candidate := range seats {
		if string(candidate) == string(key) {
			return true
		}
	}
	return false
}
