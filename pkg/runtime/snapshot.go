package runtime

import (
	"context"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

// TableSnapshot is detached from runtime state. Seated means roster/seat
// agreement only; deposits and game-specific readiness are separate facts.
type TableSnapshot struct {
	Record   TableRecord
	Phase    string
	Seats    map[uint32]string
	Payments []spend.Record
	Deposits []DepositStatus
}
type DepositStatus struct {
	Purpose               string
	Seat                  uint32
	Outpoint              string
	Atoms                 int64
	Confirmations         int64
	RequiredConfirmations int64
	AuthorityID           string
	AuthorityState        string
	Check                 string // unchecked, missing, mismatch, confirming, verified, spending, spent, unavailable
	Error                 string
}

// Snapshot exposes immutable lifecycle state without chain I/O. RefreshDeposits
// independently checks known outputs when a UI needs confirmation progress.
func (r *Runtime) Snapshot(match string) (TableSnapshot, error) {
	t, err := r.rawTable(match)
	if err != nil {
		return TableSnapshot{}, err
	}
	r.mu.Lock()
	out := TableSnapshot{Record: cloneRecord(r.snapshotLocked(t)), Phase: "admission", Seats: map[uint32]string{}}
	if t.formation() != nil {
		out.Phase = t.formation().State().String()
		if seats, ok := t.formation().Seats(); ok {
			out.Phase = "seated"
			for seat, key := range seats {
				out.Seats[seat] = hex.EncodeToString(key)
			}
		}
	}
	if t.recoveryOnly {
		out.Phase = "recovery"
	}
	r.mu.Unlock()
	for _, rec := range r.book.All() {
		if rec.Match == match {
			out.Payments = append(out.Payments, rec)
		}
	}
	if out.Record.SeatBond.Outpoint != "" {
		out.Deposits = append(out.Deposits, DepositStatus{Purpose: "seatbond", Outpoint: out.Record.SeatBond.Outpoint, Atoms: out.Record.SeatBond.Atoms, RequiredConfirmations: int64(escrow.BondConfirmations), Check: "unchecked"})
	}
	for _, group := range []struct {
		purpose       string
		values        map[uint32]Funded
		confirmations int64
	}{{"stake", out.Record.Funded, int64(escrow.StakeConfirmations)}} {
		for seat, value := range group.values {
			out.Deposits = append(out.Deposits, DepositStatus{Purpose: group.purpose, Seat: seat, Outpoint: value.Outpoint, Atoms: value.Atoms, RequiredConfirmations: group.confirmations, Check: "unchecked"})
		}
	}
	sort.Slice(out.Deposits, func(i, j int) bool {
		a, b := out.Deposits[i], out.Deposits[j]
		if a.Purpose != b.Purpose {
			return a.Purpose < b.Purpose
		}
		return a.Seat < b.Seat
	})
	return out, nil
}

// RefreshDeposits checks existence, recorded value and maturity. Script identity
// is rederived for each output; unavailable checks never imply readiness.
func (r *Runtime) RefreshDeposits(ctx context.Context, match string) (TableSnapshot, error) {
	state, e := r.bridge.FinancialState(ctx, match)
	if e != nil {
		return TableSnapshot{}, e
	}
	if state.GetClosed() {
		t, e := r.rawTable(match)
		if e != nil {
			return TableSnapshot{}, e
		}
		r.mu.Lock()
		t.recoveryOnly = true
		t.recoveryReason = "Table closed in dcrpulse; funds remain in Gaming → Recovery"
		r.mu.Unlock()
		if e = r.keep(t); e != nil {
			return TableSnapshot{}, e
		}
	}

	snap, err := r.Snapshot(match)
	if err != nil {
		return snap, err
	}
	t, err := r.rawTable(match)
	if err != nil {
		return snap, err
	}
	authority := make(map[string]*gamingpb.DepositStatus, len(state.GetDeposits()))
	for _, deposit := range state.GetDeposits() {
		if deposit != nil && deposit.GetOutpoint() != "" {
			authority[deposit.GetKind()+"\x00"+deposit.GetOutpoint()] = deposit
		}
	}
	for i := range snap.Deposits {
		dep := &snap.Deposits[i]
		bridgeDeposit := authority[dep.Purpose+"\x00"+dep.Outpoint]
		if bridgeDeposit != nil {
			dep.AuthorityID = bridgeDeposit.GetId()
			dep.AuthorityState = bridgeDeposit.GetState()
		}
		txid, vout, err := splitOutpoint(dep.Outpoint)
		if err != nil {
			dep.Check = "mismatch"
			dep.Error = err.Error()
			continue
		}
		out, err := r.bridge.UnconfirmedOutpoint(ctx, txid, vout)
		if err != nil {
			dep.Check = "unavailable"
			dep.Error = err.Error()
			continue
		}
		dep.Confirmations = out.Confirmations
		if !out.Found {
			switch dep.AuthorityState {
			case "spent":
				dep.Check = "spent"
			case "recovery_pending", "spend_pending":
				dep.Check = "spending"
			case "needs_attention":
				dep.Check = "unavailable"
				dep.Error = "dcrpulse requires operator attention for this deposit"
			default:
				dep.Check = "missing"
			}
			continue
		}
		if dep.AuthorityState == "spent" {
			dep.Check = "mismatch"
			dep.Error = "dcrpulse reports this deposit spent, but the output is still unspent"
			continue
		}
		var script string
		switch dep.Purpose {
		case "seatbond":
			redeem, e := r.seatBondScript(snap.Record.Terms)
			err = e
			if err == nil {
				script, _, err = membership.PkScriptAndAddr(redeem, r.params)
			}
		case "stake":
			var d deposit
			d, err = r.depositFor(t, dep.Seat)
			script = d.pkScript

		}
		if err != nil || !strings.EqualFold(script, out.PkScriptHex) || out.ValueAtoms != dep.Atoms {
			dep.Check = "mismatch"
			if err != nil {
				dep.Error = err.Error()
			}
			continue
		}
		dep.Check = "confirming"
		if dep.Confirmations >= dep.RequiredConfirmations {
			dep.Check = "verified"
		}
	}
	return snap, nil
}
