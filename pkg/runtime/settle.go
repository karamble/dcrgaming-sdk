package runtime

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/wire"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
)

// settleFee is what a settlement pays when the game says nothing. It falls on
// the seats evenly with the remainder on the first, which has to be a rule
// rather than a preference: two peers rounding differently produce signatures
// that cannot be combined.
const settleFee = 20_000

// Settle declares how a finished table's money is divided.
//
// The runtime does not check the outcome, because it cannot: the rules that
// produced it are the game's, and a runtime that second-guessed them would be a
// runtime with an opinion about who won. What it does check is everything the
// outcome has to be consistent with - that the shares name real seats, that
// they add up to what the table actually holds, and that nobody is paid to an
// address they never announced.
//
// Those checks are not politeness. A settlement is co-signed by every seat, so
// one that does not add up is one the other seats refuse, and a table whose
// seats cannot agree a payout falls back to its refund timelocks - which is
// weeks of everybody's money locked up over an arithmetic mistake.
func (r *Runtime) Settle(ctx context.Context, match string, out Outcome) error {
	if match == "" {
		return fmt.Errorf("settling needs a table")
	}
	if !out.Void && len(out.Shares) == 0 {
		return fmt.Errorf("an outcome that is neither void nor a share of anything settles nothing")
	}
	draft, err := r.settleDraft(match, out)
	if err != nil {
		return err
	}
	tx, err := escrow.BuildSettlement(draft)
	if err != nil {
		return fmt.Errorf("build the settlement: %w", err)
	}
	_ = tx
	// Co-signing and broadcast need every other seat's signature to come
	// back over the wire, which is the half that waits on the funding and
	// dispatch stages. The draft above is built and checked here so a game
	// integrating now finds out about a bad outcome immediately.
	return fmt.Errorf("co-signing the settlement: %w", ErrNotYet)
}

// settleDraft turns an outcome into the transaction every seat will sign.
//
// Built in seat order, because that is what decides the transaction's bytes and
// every peer has to build the same ones. A map iterated in Go's order would
// give two honest peers two different transactions.
func (r *Runtime) settleDraft(match string, out Outcome) (escrow.SettleDraft, error) {
	r.mu.Lock()
	t, ok := r.tables[match]
	r.mu.Unlock()
	if !ok {
		return escrow.SettleDraft{}, fmt.Errorf("no table %q", match)
	}
	seats, ok := t.form.Seats()
	if !ok {
		return escrow.SettleDraft{}, fmt.Errorf("this table has no seating yet")
	}
	deposits, err := t.form.Deposits(r.params)
	if err != nil {
		return escrow.SettleDraft{}, err
	}
	bySeat := make(map[uint32]string, len(deposits))
	for _, d := range deposits {
		bySeat[d.Seat] = d.RedeemScriptHex
	}

	order := seatOrder(seats)

	shares, err := r.sharesFor(t, order, out)
	if err != nil {
		return escrow.SettleDraft{}, err
	}

	draft := escrow.SettleDraft{FeeAtoms: settleFee}
	for _, seat := range order {
		redeem, err := hex.DecodeString(bySeat[seat])
		if err != nil || len(redeem) == 0 {
			return escrow.SettleDraft{}, fmt.Errorf("seat %d has no escrow script", seat)
		}
		staked, ok := t.funded[seat]
		if !ok || staked.outpoint == "" {
			return escrow.SettleDraft{}, fmt.Errorf("seat %d's stake is not on the chain yet: %w",
				seat, ErrNotYet)
		}
		prevout, err := outpointOf(staked.outpoint)
		if err != nil {
			return escrow.SettleDraft{}, fmt.Errorf("seat %d: %w", seat, err)
		}
		pay, ok := t.payouts[seat]
		if !ok || len(pay) == 0 {
			return escrow.SettleDraft{}, fmt.Errorf("seat %d has not said where to pay it", seat)
		}
		draft.Inputs = append(draft.Inputs, escrow.SettleInput{
			Redeem: redeem, Prevout: prevout, ValueAtoms: staked.atoms,
		})
		draft.Pays = append(draft.Pays, pay)
		draft.Amounts = append(draft.Amounts, shares[seat])
	}
	return draft, nil
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
		held += t.funded[seat].atoms
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
		if amount < 0 {
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
