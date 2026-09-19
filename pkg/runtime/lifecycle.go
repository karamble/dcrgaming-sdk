package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

var ErrNotSeated = errors.New("table has not seated yet")
var ErrUnresolvedPayment = errors.New("payment dispatch unresolved: reconcile the bridge request id; do not pay again")

// InviteResolver validates game-specific requirements after copying the exact
// advertised economics. It cannot replace operator-approved financial terms.
type InviteResolver interface {
	ResolveInvite(context.Context, schema.Invite, membership.Terms) (membership.Terms, error)
}

func (r *Runtime) resolveInvite(ctx context.Context, inv schema.Invite) (membership.Terms, error) {
	terms, err := r.termsFor(inv)
	if err != nil {
		return terms, err
	}
	if hook, ok := r.rules.(InviteResolver); ok {
		original := terms
		terms, err = hook.ResolveInvite(ctx, inv, terms)
		if err != nil {
			return terms, err
		}
		if terms.BondAtoms != inv.AdmissionAtoms || terms.BondLockBlocks != inv.AdmissionBlocks || terms.Game != original.Game || terms.GameVer != original.GameVer || terms.SID != original.SID || (inv.BuyInAtoms != 0 && terms.BuyInAtoms != inv.BuyInAtoms) || (inv.Seats != 0 && terms.Seats != inv.Seats) || (inv.CSVBlocks != 0 && terms.CSVBlocks != inv.CSVBlocks) || (inv.Until != 0 && terms.Until != inv.Until) {
			return terms, fmt.Errorf("game attempted to replace advertised invitation terms")
		}
	}
	return terms, terms.Validate()
}
func (r *Runtime) healthy() error {
	r.lifeMu.Lock()
	stopped := r.stopping
	r.lifeMu.Unlock()
	if stopped {
		return fmt.Errorf("runtime stopped")
	}
	if err := r.book.Err(); err != nil {
		return r.storageFault(err)
	}
	r.faultMu.Lock()
	defer r.faultMu.Unlock()
	return r.storageErr
}
func (r *Runtime) storageFault(err error) error {
	r.faultMu.Lock()
	defer r.faultMu.Unlock()
	if r.storageErr == nil {
		r.storageErr = fmt.Errorf("runtime storage fault: %w", err)
		close(r.faultCh)
	}
	return r.storageErr
}
func cloneStrings(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
func (r *Runtime) startAdmission(t *table) {
	r.lifeMu.Lock()
	defer r.lifeMu.Unlock()
	if !r.running || r.stopping || r.admissionWorkers[t.match] {
		return
	}
	r.mu.Lock()
	pending := t.formation() == nil && !t.recoveryOnly
	r.mu.Unlock()
	if !pending {
		return
	}
	r.admissionWorkers[t.match] = true
	r.workers.Add(1)
	ctx := r.runCtx
	go func() {
		defer r.workers.Done()
		defer func() { r.lifeMu.Lock(); delete(r.admissionWorkers, t.match); r.lifeMu.Unlock() }()
		r.joinWhenBonded(ctx, t)
	}()
}
func (r *Runtime) fundingLock(match, purpose string) *sync.Mutex {
	r.fundingMu.Lock()
	defer r.fundingMu.Unlock()
	if r.obligationLocks == nil {
		r.obligationLocks = map[string]*sync.Mutex{}
	}
	key := match + "\x00" + purpose
	l := r.obligationLocks[key]
	if l == nil {
		l = &sync.Mutex{}
		r.obligationLocks[key] = l
	}
	return l
}

// ResumeReport distinguishes retained recovery records from usable tables.
type ResumeReport struct {
	Restored, RecoveryOnly []string
	Failed                 map[string]string
}

// Resumed is what the last resume found on disk, for a caller of [Open] that
// wants to see it without resuming again.
func (r *Runtime) Resumed() ResumeReport {
	return ResumeReport{Restored: append([]string(nil), r.resumeReport.Restored...), RecoveryOnly: append([]string(nil), r.resumeReport.RecoveryOnly...), Failed: cloneStrings(r.resumeReport.Failed)}
}

func (r *Runtime) ResumeWithReport() (ResumeReport, error) {
	err := r.Resume()
	report := ResumeReport{Restored: append([]string(nil), r.resumeReport.Restored...), RecoveryOnly: append([]string(nil), r.resumeReport.RecoveryOnly...), Failed: cloneStrings(r.resumeReport.Failed)}
	return report, err
}

// ReconcileSpend attaches an operator-supplied id after checking the bridge's
// authoritative record. This never issues RequestSpend.
func (r *Runtime) ReconcileSpend(ctx context.Context, match, purpose, id string) error {
	lock := r.fundingLock(match, purpose)
	lock.Lock()
	defer lock.Unlock()
	if err := r.healthy(); err != nil {
		return err
	}
	var pending *spend.Record
	for _, rec := range r.book.All() {
		if rec.Match == match && rec.Purpose == purpose && rec.ID == "" {
			if pending != nil {
				return fmt.Errorf("ambiguous pending obligations")
			}
			c := rec
			pending = &c
		}
	}
	if pending == nil {
		return fmt.Errorf("no unresolved obligation")
	}
	sp, err := r.bridge.SpendStatus(ctx, id)
	if err != nil {
		return err
	}
	if sp.ID != id || sp.Game != r.rules.Identity().GameID || sp.Address != pending.Address || sp.AmountAtoms != pending.Atoms || sp.Reason != pending.Purpose {
		return fmt.Errorf("bridge request does not match obligation")
	}
	if _, err = r.book.Adopt(match, purpose, id); err != nil {
		return r.storageFault(err)
	}
	if _, err = r.book.Note(id, sp, nil); err != nil {
		return r.storageFault(err)
	}
	t, err := r.rawTable(match)
	if err == nil {
		r.startAdmission(t)
	}
	return err
}

// RetryFunding is an explicit new attempt after a bridge-confirmed unpaid
// refusal or expiry. Unknown dispatches and failed payments cannot be retried.
func (r *Runtime) RetryFunding(ctx context.Context, id string) (spend.Record, error) {
	old, ok := r.book.Get(id)
	if !ok {
		return spend.Record{}, fmt.Errorf("unknown request")
	}
	lock := r.fundingLock(old.Match, old.Purpose)
	lock.Lock()
	defer lock.Unlock()
	if err := r.healthy(); err != nil {
		return spend.Record{}, err
	}
	sp, err := r.bridge.SpendStatus(ctx, id)
	if err != nil {
		return spend.Record{}, err
	}
	if sp.ID != id || sp.Game != r.rules.Identity().GameID || sp.Address != old.Address || sp.AmountAtoms != old.Atoms || sp.Reason != old.Purpose {
		return spend.Record{}, fmt.Errorf("bridge request mismatch")
	}
	if sp.TxID != "" || (sp.State != "denied" && sp.State != "expired") {
		return spend.Record{}, fmt.Errorf("payment is not proven unpaid")
	}
	if old.Purpose == "seatbond" {
		tip, err := r.bridge.ChainTip(ctx)
		if err != nil {
			return spend.Record{}, err
		}
		if tip.Height > int64(r.Terms(old.Match).Until) {
			return spend.Record{}, fmt.Errorf("admission deadline passed")
		}
	}
	if _, err = r.book.Note(id, sp, nil); err != nil {
		return spend.Record{}, r.storageFault(err)
	}
	next, err := r.book.Retry(id)
	if err != nil {
		return spend.Record{}, err
	}
	return r.dispatchSpend(ctx, next)
}
