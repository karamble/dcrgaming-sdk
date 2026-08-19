package runtime

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

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

// Release hands this seat's table bond back cooperatively, which is what a
// match ending with nothing to punish comes to.
//
// Only this seat's own: there is no argument for whose, because a release pays
// its owner and nothing else, so there is nothing to name.
//
// Both seats sign, because the bond's cooperative branch needs both. If the
// other seat will not, the money is not stuck: the same bond has a backstop
// branch its owner can spend alone once the lock matures, which is what makes
// withholding a signature pointless rather than profitable.
func (r *Runtime) Release(ctx context.Context, match string) error {
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
	if t.releases == nil {
		t.releases = map[uint32]*release{}
	}
	if t.releases[mine] == nil {
		t.releases[mine] = &release{seat: mine, tx: tx, draft: draft, sigs: map[string][]byte{}}
	}
	t.releases[mine].sigs[hex.EncodeToString(seats[mine])] = sig
	r.mu.Unlock()

	raw, err := tx.Bytes()
	if err != nil {
		return err
	}
	body := schema.Release{
		Seat: mine, Tx: hex.EncodeToString(raw), Signer: hex.EncodeToString(seats[mine]),
		Sig: hex.EncodeToString(sig),
	}
	if err := r.send(ctx, t, KindRelease, body); err != nil {
		return fmt.Errorf("tell the table about the release: %w", err)
	}
	return r.completeRelease(ctx, t, mine)
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

// adoptRelease co-signs another seat's release, or takes their signature on
// this seat's.
//
// Which of the two it is comes from the seat the message names, and both are
// ordinary: a table ends for everybody at once, so both seats release at the
// same moment and each holds two conversations - its own release waiting for
// the other's signature, and the other's waiting for its own.
//
// The transaction is rebuilt here rather than believed. A release names where
// it pays, and a seat that could get its opponent to sign an arbitrary
// transaction spending a bond has been handed the bond.
func (r *Runtime) adoptRelease(ctx context.Context, match string, body schema.Release) error {
	t, mine, err := r.ourSeatAt(match)
	if err != nil {
		return err
	}
	seats, ok := t.form.Seats()
	if !ok {
		return fmt.Errorf("this table has no seating yet")
	}
	signer, err := hex.DecodeString(body.Signer)
	if err != nil || !seatedKey(seats, signer) {
		return fmt.Errorf("a release was signed by somebody who is not at this table")
	}
	sig, err := hex.DecodeString(body.Sig)
	if err != nil || len(sig) == 0 {
		return fmt.Errorf("a release signature is not usable")
	}

	// What this peer would have built for that seat, which is what it is
	// willing to sign. Anything else is refused rather than signed.
	draft, err := r.releaseDraft(t, body.Seat)
	if err != nil {
		return err
	}
	want, err := punish.BuildRelease(draft)
	if err != nil {
		return fmt.Errorf("build seat %d's release: %w", body.Seat, err)
	}
	raw, err := want.Bytes()
	if err != nil {
		return err
	}
	if !strings.EqualFold(hex.EncodeToString(raw), body.Tx) {
		return fmt.Errorf(
			"seat %d proposed a release this peer would not have built, and it was not signed", body.Seat)
	}

	r.mu.Lock()
	if t.releases == nil {
		t.releases = map[uint32]*release{}
	}
	held := t.releases[body.Seat]
	if held == nil {
		held = &release{seat: body.Seat, tx: want, draft: draft, sigs: map[string][]byte{}}
		t.releases[body.Seat] = held
	}
	held.sigs[body.Signer] = sig
	_, alsoOurs := held.sigs[hex.EncodeToString(seats[mine])]
	r.mu.Unlock()

	// Somebody else's release still needs this seat's signature on it, and
	// this is the moment to give it: the bond is theirs, it pays only them,
	// and withholding costs them a wait and gains nothing.
	if body.Seat != mine && !alsoOurs {
		session, _, err := r.seatKeys(t.form.Terms().SID)
		if err != nil {
			return err
		}
		ours, err := escrow.SignBondSpend(want, draft.Bond, session)
		if err != nil {
			return fmt.Errorf("sign seat %d's release: %w", body.Seat, err)
		}
		r.mu.Lock()
		held.sigs[hex.EncodeToString(seats[mine])] = ours
		r.mu.Unlock()
		if err := r.send(ctx, t, KindRelease, schema.Release{
			Seat: body.Seat, Tx: body.Tx, Signer: hex.EncodeToString(seats[mine]),
			Sig: hex.EncodeToString(ours),
		}); err != nil {
			return fmt.Errorf("tell the table about the release: %w", err)
		}
	}
	return r.completeRelease(ctx, t, body.Seat)
}

// completeRelease sends the release once both seats have signed.
func (r *Runtime) completeRelease(ctx context.Context, t *table, seat uint32) error {
	r.mu.Lock()
	rel := t.releases[seat]
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
	return r.sendRelease(ctx, t, seat, final)
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
	return r.sendRelease(ctx, t, mine, tx)
}

// sendRelease broadcasts a finished release, once.
func (r *Runtime) sendRelease(ctx context.Context, t *table, seat uint32, tx *wire.MsgTx) error {
	r.mu.Lock()
	rel := t.releases[seat]
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
	seat  uint32
	tx    *wire.MsgTx
	draft punish.Release
	sigs  map[string][]byte
	done  bool
}
