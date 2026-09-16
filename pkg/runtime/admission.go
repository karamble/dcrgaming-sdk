package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

func (r *Runtime) checkAdmissionBond(ctx context.Context, terms membership.Terms, j *membership.Join) error {
	id, vout, err := splitOutpoint(j.Bond.Outpoint)
	if err != nil {
		return err
	}
	out, err := r.bridge.Outpoint(ctx, id, vout)
	if err != nil {
		return err
	}
	if err = membership.CheckBond(j, membership.BondFacts{Found: out.Found, ValueAtoms: out.ValueAtoms, PkScriptHex: out.PkScriptHex, Confirmations: out.Confirmations}, r.params); err != nil {
		return err
	}
	if out.ValueAtoms != int64(terms.BondAtoms) {
		return fmt.Errorf("admission bond differs from agreed value")
	}
	return nil
}

// CheckAdmissionBonds independently verifies the complete roster's admission
// deposits against the chain. Membership signatures alone do not prove funding.
func (r *Runtime) CheckAdmissionBonds(ctx context.Context, match string) error {
	t, err := r.tableOf(match)
	if err != nil {
		return err
	}
	f := t.formation()
	joins := f.Joins()
	if len(joins) != int(t.terms.Seats) {
		return fmt.Errorf("admission roster incomplete")
	}
	for _, j := range joins {
		if err = r.checkAdmissionBond(ctx, t.terms, j); err != nil {
			return err
		}
	}
	return nil
}
func (r *Runtime) awaitAdmissionBond(ctx context.Context, t *table, j *membership.Join) error {
	timer := time.NewTicker(fundPoll)
	defer timer.Stop()
	for {
		tip, err := r.bridge.ChainTip(ctx)
		if err != nil {
			return err
		}
		if tip.Height > int64(t.terms.Until) {
			r.mu.Lock()
			t.recoveryOnly = true
			t.recoveryReason = "admission expired before bond confirmation"
			r.mu.Unlock()
			if err = r.keep(t); err != nil {
				return err
			}
			return fmt.Errorf("admission expired; deposit retained for recovery")
		}
		if err = r.checkAdmissionBond(ctx, t.terms, j); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}
