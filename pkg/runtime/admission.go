package runtime

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
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

// checkAdmissionBondAnnouncement verifies the immutable output named by a join
// as soon as it is visible in the mempool or chain. Confirmation depth is a
// later readiness condition; making it an admission condition creates an
// impossible race between a short registration window and two confirmations.
func (r *Runtime) checkAdmissionBondAnnouncement(ctx context.Context, terms membership.Terms, j *membership.Join) error {
	if j == nil {
		return fmt.Errorf("no join")
	}
	id, vout, err := splitOutpoint(j.Bond.Outpoint)
	if err != nil {
		return err
	}
	out, err := r.bridge.UnconfirmedOutpoint(ctx, id, vout)
	if err != nil {
		return err
	}
	if !out.Found {
		return fmt.Errorf("bond deposit %s is not visible in the mempool or chain", j.Bond.Outpoint)
	}
	_, pkScript, err := escrow.BondAddress(j.Bond.Script, r.params)
	if err != nil {
		return fmt.Errorf("bond address: %w", err)
	}
	if !strings.EqualFold(out.PkScriptHex, hex.EncodeToString(pkScript)) {
		return fmt.Errorf("bond deposit %s does not pay the announced bond script", j.Bond.Outpoint)
	}
	if out.ValueAtoms != int64(terms.BondAtoms) {
		return fmt.Errorf("bond holds %d atoms, expected %d", out.ValueAtoms, terms.BondAtoms)
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
