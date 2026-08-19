package runtime

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/decred/dcrd/wire"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
	"github.com/karamble/dcrgaming-sdk/pkg/punish"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

// KindRelease carries a co-signed release of a table bond. The runtime's own.
const KindRelease = schema.KindRelease

// FundTableBond pays this seat's table bond, which is what it stakes against
// staying reachable.
func (r *Runtime) FundTableBond(ctx context.Context, match string) error {
	t, mine, err := r.ourSeatAt(match)
	if err != nil {
		return err
	}
	bond, err := r.tableBondOf(t, mine)
	if err != nil {
		return err
	}
	r.mu.Lock()
	already, funded := t.tableBondFunded[mine]
	r.mu.Unlock()
	if funded && already.outpoint != "" {
		return nil
	}
	atoms := int64(t.form.Terms().BondAtoms)
	if atoms <= 0 {
		return fmt.Errorf("this table states no bond amount")
	}
	rec, err := r.askFor(ctx, spend.Record{
		Match: match, Seat: mine, Purpose: "tablebond",
		Address: bond.Address, Atoms: atoms, PkScript: bond.PkScriptHex,
	})
	if err != nil {
		return err
	}
	out, err := r.findOutput(ctx, rec)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if t.tableBondFunded == nil {
		t.tableBondFunded = map[uint32]staked{}
	}
	t.tableBondFunded[mine] = out
	r.mu.Unlock()
	r.keep(t)
	if err := r.announceBonded(ctx, match, tableBond); err != nil {
		r.log.Warnf("table %s: saying where the bond is: %v", match, err)
	}
	return nil
}

func (r *Runtime) tableBondOf(t *table, seat uint32) (membership.TableBond, error) {
	bonds, err := t.form.TableBonds(r.params)
	if err != nil {
		return membership.TableBond{}, err
	}
	for _, b := range bonds {
		if b.Seat == seat {
			return b, nil
		}
	}
	return membership.TableBond{}, fmt.Errorf("this table has no bond for seat %d", seat)
}

// releaseTableBond hands a seat's table bond back cooperatively.
//
// Both seats sign, because the bond's cooperative branch needs both. If the
// other seat will not, the money is not stuck: the same bond has a backstop
// branch its owner can spend alone once the lock matures, which is what makes
// withholding a signature pointless rather than profitable.
func (r *Runtime) releaseTableBond(ctx context.Context, match string) error {
	t, mine, err := r.ourSeatAt(match)
	if err != nil {
		return err
	}
	draft, err := r.releaseDraft(t, mine)
	if err != nil {
		return err
	}
	tx, err := punish.BuildRelease(draft)
	if err != nil {
		return fmt.Errorf("build the release: %w", err)
	}
	session, _, err := r.seatKeys(t.form.Terms().SID)
	if err != nil {
		return err
	}
	sig, err := escrow.SignBondSpend(tx, draft.Bond, session)
	if err != nil {
		return fmt.Errorf("sign the release: %w", err)
	}

	seats, _ := t.form.Seats()
	r.mu.Lock()
	if t.release == nil {
		t.release = &release{tx: tx, draft: draft, sigs: map[string][]byte{}}
	}
	t.release.sigs[hex.EncodeToString(seats[mine])] = sig
	r.mu.Unlock()

	raw, err := tx.Bytes()
	if err != nil {
		return err
	}
	body := schema.Release{
		Tx: hex.EncodeToString(raw), Signer: hex.EncodeToString(seats[mine]),
		Sig: hex.EncodeToString(sig),
	}
	if err := r.send(ctx, t, KindRelease, body); err != nil {
		return fmt.Errorf("tell the table about the release: %w", err)
	}
	return r.completeRelease(ctx, t)
}

// releaseDraft is this seat's table bond going home.
func (r *Runtime) releaseDraft(t *table, seat uint32) (punish.Release, error) {
	bond, err := r.tableBondOf(t, seat)
	if err != nil {
		return punish.Release{}, err
	}
	script, err := hex.DecodeString(bond.ScriptHex)
	if err != nil || len(script) == 0 {
		return punish.Release{}, fmt.Errorf("seat %d's table bond has no script", seat)
	}
	r.mu.Lock()
	funded, ok := t.tableBondFunded[seat]
	pay := t.payouts[seat]
	r.mu.Unlock()
	if !ok || funded.outpoint == "" {
		return punish.Release{}, fmt.Errorf("seat %d's table bond is not on the chain", seat)
	}
	if len(pay) == 0 {
		return punish.Release{}, fmt.Errorf("seat %d has not said where to pay it", seat)
	}
	prevout, err := outpointOf(funded.outpoint)
	if err != nil {
		return punish.Release{}, err
	}
	return punish.Release{
		Bond: script, Prevout: prevout, ValueAtoms: funded.atoms,
		OwnerPay: pay, FeeAtoms: r.reclaimFee, Params: r.params,
	}, nil
}

// adoptRelease takes the other seat's signature on a release.
func (r *Runtime) adoptRelease(ctx context.Context, match string, body schema.Release) error {
	r.mu.Lock()
	t, ok := r.tables[match]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("no table %q", match)
	}
	seats, ok := t.form.Seats()
	if !ok {
		return fmt.Errorf("this table has no seating yet")
	}
	signer, err := hex.DecodeString(body.Signer)
	if err != nil || !seatedKey(seats, signer) {
		return fmt.Errorf("a release was signed by somebody who is not at this table")
	}
	r.mu.Lock()
	held := t.release
	r.mu.Unlock()
	if held == nil {
		return fmt.Errorf("no release has been proposed at this table, so there is nothing to agree with")
	}
	// The transaction is this peer's own: a release pays its owner and
	// nobody else, so there is nothing to negotiate and nothing to compare
	// beyond the signature itself.
	sig, err := hex.DecodeString(body.Sig)
	if err != nil || len(sig) == 0 {
		return fmt.Errorf("a release signature is not usable")
	}
	r.mu.Lock()
	held.sigs[body.Signer] = sig
	r.mu.Unlock()
	return r.completeRelease(ctx, t)
}

// completeRelease sends the release once both seats have signed.
func (r *Runtime) completeRelease(ctx context.Context, t *table) error {
	r.mu.Lock()
	rel := t.release
	if rel == nil || rel.done {
		r.mu.Unlock()
		return nil
	}
	members, err := escrow.Members(rel.draft.Bond)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	ordered := make([][]byte, 0, len(members))
	for _, m := range members {
		sig, ok := rel.sigs[hex.EncodeToString(m)]
		if !ok {
			r.mu.Unlock()
			return nil // still short of somebody
		}
		ordered = append(ordered, sig)
	}
	tx, bond := rel.tx, rel.draft.Bond
	r.mu.Unlock()

	final, err := punish.CoSignRelease(tx, bond, ordered, r.params)
	if err != nil {
		return fmt.Errorf("a fully signed release did not satisfy the bond: %w", err)
	}
	return r.sendRelease(ctx, t, final)
}

// BackstopRelease takes this seat's own table bond home alone, once the lock
// has matured.
//
// The reason withholding a co-signature is pointless: the money comes back
// either way, only later.
func (r *Runtime) BackstopRelease(ctx context.Context, match string) error {
	t, mine, err := r.ourSeatAt(match)
	if err != nil {
		return err
	}
	draft, err := r.releaseDraft(t, mine)
	if err != nil {
		return err
	}
	session, _, err := r.seatKeys(t.form.Terms().SID)
	if err != nil {
		return err
	}
	tx, err := punish.BackstopRelease(session, draft)
	if err != nil {
		return fmt.Errorf("build the backstop: %w", err)
	}
	return r.sendRelease(ctx, t, tx)
}

// sendRelease broadcasts a finished release, once.
func (r *Runtime) sendRelease(ctx context.Context, t *table, tx *wire.MsgTx) error {
	r.mu.Lock()
	rel := t.release
	if rel != nil {
		if rel.done {
			r.mu.Unlock()
			return nil
		}
		rel.done = true
	}
	r.mu.Unlock()

	raw, err := tx.Bytes()
	if err != nil {
		return err
	}
	txid, err := r.bridge.Broadcast(ctx, hex.EncodeToString(raw))
	if err != nil {
		return fmt.Errorf("send the release: %w", err)
	}
	r.log.Infof("table %s: table bond released in %s", t.match, txid)
	return nil
}

// release is a table bond going home cooperatively, part-signed.
type release struct {
	tx    *wire.MsgTx
	draft punish.Release
	sigs  map[string][]byte
	done  bool
}
