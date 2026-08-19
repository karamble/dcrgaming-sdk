package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

// maxFundingVout bounds the search for a deposit in the transaction that paid
// it. The bridge builds these, so a stake is never buried deep; searching
// without a bound would let a malformed answer spin here forever.
const maxFundingVout = 16

// fundPoll is how often a pending request is asked about again. A person is
// being waited on, so it is patient. A variable only so a test need not wait.
var fundPoll = 3 * time.Second

// Fund pays this seat's stake into the table's escrow and waits for it to land.
//
// The order is deliberate and it is the part worth copying if you ever write
// something like this yourself:
//
//  1. Write the request down.
//  2. Ask the bridge.
//  3. Record the id it issues.
//  4. Keep asking until a person answers.
//  5. Find the output, and only then call it funded.
//
// Step 1 comes before step 2 because a request the bridge received but this
// process never wrote down is a payment nobody is watching. A record of
// something that never happened is discovered by asking; a payment with no
// record is discovered by an accountant.
//
// Step 4 never gives up because the bridge could not be reached. That is not an
// answer - the money may already have moved - and treating it as one is how a
// stake gets paid twice. See pkg/spend.
func (r *Runtime) Fund(ctx context.Context, match string) error {
	r.mu.Lock()
	t, ok := r.tables[match]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("no table %q", match)
	}
	seat, ok := t.form.OurSeat()
	if !ok {
		return fmt.Errorf("this table has not seated us yet")
	}
	r.mu.Lock()
	already, funded := t.funded[seat]
	r.mu.Unlock()
	if funded && already.outpoint != "" {
		// Paying again would be paying twice.
		return nil
	}

	dep, err := r.depositFor(t, seat)
	if err != nil {
		return err
	}
	terms := t.form.Terms()
	rec, err := r.askFor(ctx, spend.Record{
		Match: match, Seat: seat, Purpose: "stake",
		Address: dep.addr, Atoms: int64(terms.BuyInAtoms), PkScript: dep.pkScript,
	})
	if err != nil {
		return err
	}
	return r.landDeposit(ctx, t, seat, rec)
}

// deposit is where one seat's stake has to be paid.
type deposit struct {
	addr     string
	pkScript string
}

// depositFor is the escrow address this seat's stake belongs in.
func (r *Runtime) depositFor(t *table, seat uint32) (deposit, error) {
	deposits, err := t.form.Deposits(r.params)
	if err != nil {
		return deposit{}, err
	}
	for _, d := range deposits {
		if d.Seat != seat {
			continue
		}
		if d.DepositAddr == "" || d.PkScriptHex == "" {
			return deposit{}, fmt.Errorf("seat %d has an escrow with no address", seat)
		}
		return deposit{addr: d.DepositAddr, pkScript: d.PkScriptHex}, nil
	}
	return deposit{}, fmt.Errorf("this table has no escrow for seat %d", seat)
}

// askFor writes a request down, asks the bridge, and waits for a person.
func (r *Runtime) askFor(ctx context.Context, want spend.Record) (spend.Record, error) {
	if err := r.book.Put(want); err != nil {
		return spend.Record{}, err
	}
	sp, err := r.bridge.RequestSpend(ctx, want.Address, want.Atoms, want.Purpose)
	if err != nil {
		// The bridge may have taken the request even though the answer did
		// not come back. The record stays, unidentified, and OpenRecords
		// reports it so a resume can ask about it rather than paying again.
		return spend.Record{}, fmt.Errorf("ask for the %s: %w", want.Purpose, err)
	}
	rec, err := r.book.Adopt(want.Match, want.Purpose, sp.ID)
	if err != nil {
		return spend.Record{}, err
	}
	if _, err := r.book.Note(rec.ID, sp, nil); err != nil {
		return spend.Record{}, err
	}
	return r.awaitAnswer(ctx, rec.ID)
}

// awaitAnswer keeps asking until somebody decides.
//
// An unreachable bridge is recorded and asked again; it never ends the wait,
// because it says only that this process could not ask. Only an answer, or the
// caller's own context, ends this.
func (r *Runtime) awaitAnswer(ctx context.Context, id string) (spend.Record, error) {
	ticker := time.NewTicker(fundPoll)
	defer ticker.Stop()

	for {
		got, ok := r.book.Get(id)
		switch {
		case !ok:
			return spend.Record{}, fmt.Errorf("no request is recorded as %q", id)
		case got.State == spend.Approved || got.State == spend.Located:
			return got, nil
		case got.State.Terminal():
			return got, fmt.Errorf("the %s was not paid (%s): %s",
				got.Purpose, got.State, got.Error)
		}

		sp, err := r.bridge.SpendStatus(ctx, id)
		if _, nerr := r.book.Note(id, sp, err); nerr != nil && !transport.Unreachable(nerr) {
			return spend.Record{}, nerr
		}

		select {
		case <-ctx.Done():
			return spend.Record{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// landDeposit finds the output the payment made and records the seat as funded.
func (r *Runtime) landDeposit(ctx context.Context, t *table, seat uint32, rec spend.Record) error {
	if rec.TxID == "" {
		return fmt.Errorf("the %s was approved without a transaction", rec.Purpose)
	}
	for vout := range uint32(maxFundingVout + 1) {
		// The mempool counts here, unlike a reclaim: what matters is that
		// the payment exists, not how old it is. Confirmations are checked
		// later, by whatever decision needs them.
		out, err := r.bridge.UnconfirmedOutpoint(ctx, rec.TxID, vout)
		if err != nil {
			return fmt.Errorf("look for the %s output: %w", rec.Purpose, err)
		}
		if !out.Found || !strings.EqualFold(out.PkScriptHex, rec.PkScript) {
			continue
		}
		outpoint := fmt.Sprintf("%s:%d", rec.TxID, vout)
		if _, err := r.book.Locate(rec.ID, outpoint); err != nil {
			return err
		}
		r.mu.Lock()
		if t.funded == nil {
			t.funded = map[uint32]staked{}
		}
		t.funded[seat] = staked{outpoint: outpoint, atoms: out.ValueAtoms}
		r.mu.Unlock()
		return nil
	}
	return fmt.Errorf("%s %s pays no output with the script that was asked for; "+
		"the money moved but not to this table", rec.Purpose, rec.TxID)
}

// SetPayoutFor records where a seat asked to be paid, which every seat
// announces because it goes into the settlement they all sign.
func (r *Runtime) SetPayoutFor(match string, seat uint32, payScript []byte) error {
	if len(payScript) == 0 {
		return fmt.Errorf("seat %d named no payout script", seat)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tables[match]
	if !ok {
		return fmt.Errorf("no table %q", match)
	}
	if t.payouts == nil {
		t.payouts = map[uint32][]byte{}
	}
	t.payouts[seat] = append([]byte(nil), payScript...)
	return nil
}

// Funded reports where a seat's stake landed, if it has.
func (r *Runtime) Funded(match string, seat uint32) (outpoint string, atoms int64, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, found := r.tables[match]
	if !found {
		return "", 0, false
	}
	s, has := t.funded[seat]
	if !has || s.outpoint == "" {
		return "", 0, false
	}
	return s.outpoint, s.atoms, true
}
