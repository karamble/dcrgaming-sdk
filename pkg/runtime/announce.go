package runtime

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

// Telling the table where the money went.
//
// A seat pays its own stake and is the only one that saw it happen: nothing on
// the chain says which output belongs to which table, so a peer that is not
// told never learns. Every settlement spends every seat's stake, so a table
// where this does not happen cannot pay anybody - each side builds a
// settlement over the one stake it knows about and neither will sign the
// other's.
//
// Three announcements, all the same shape: a stake, a table bond, and where a
// seat wants to be paid.

// The runtime's own, alongside join, commit and settle.
const (
	KindFunded = schema.KindFunded
	KindBonded = schema.KindBonded
	KindPayout = schema.KindPayout
)

// announceFunded tells the table where this seat's stake landed.
func (r *Runtime) announceFunded(ctx context.Context, match string) error {
	t, mine, err := r.ourSeatAt(match)
	if err != nil {
		return err
	}
	r.mu.Lock()
	out, have := t.funded[mine]
	r.mu.Unlock()
	if !have || out.outpoint == "" {
		return fmt.Errorf("this seat's stake is not on the chain yet")
	}
	session, _, err := r.seatKeys(t.form.Terms().SID)
	if err != nil {
		return err
	}
	f, err := membership.SignFunding(t.form.Terms(), mine, out.outpoint, session)
	if err != nil {
		return fmt.Errorf("sign where the stake is: %w", err)
	}
	return r.send(ctx, t, KindFunded, schema.FundedFrom(f))
}

// adoptFunded takes another seat's word for where its stake is - and checks it.
//
// Signed by the seat it names, so nobody can place somebody else's stake, and
// checked against the chain, because a signature only proves who said it. The
// output has to pay the escrow this table derived for that seat: without that
// check a peer could point the settlement at an output it can spend alone, and
// every other seat would sign a transaction handing it the pot.
func (r *Runtime) adoptFunded(ctx context.Context, match string, body schema.Funded) error {
	f, err := body.Into()
	if err != nil {
		return fmt.Errorf("read where a stake is: %w", err)
	}
	t, err := r.tableOf(match)
	if err != nil {
		return err
	}
	if err := r.seatSaidIt(t, f.Seat, f.Signer); err != nil {
		return err
	}
	if err := f.Verify(t.form.Terms()); err != nil {
		return fmt.Errorf("seat %d's stake: %w", f.Seat, err)
	}
	dep, err := r.depositFor(t, f.Seat)
	if err != nil {
		return err
	}
	out, err := r.outputPaying(ctx, f.Outpoint, dep.pkScript)
	if err != nil {
		return fmt.Errorf("seat %d's stake: %w", f.Seat, err)
	}
	// The amount is the table's, not the payer's to choose. A seat that
	// staked less than the buy-in would be playing for a pot everyone else
	// filled.
	if want := int64(t.form.Terms().BuyInAtoms); out.atoms < want {
		return fmt.Errorf("seat %d's stake holds %d atoms, and this table costs %d",
			f.Seat, out.atoms, want)
	}

	r.mu.Lock()
	if t.funded == nil {
		t.funded = map[uint32]staked{}
	}
	held, seen := t.funded[f.Seat]
	if seen && held.outpoint != "" && held.outpoint != f.Outpoint {
		r.mu.Unlock()
		// Not reconciled: whichever the settlement was built over, the
		// other announcement changes what it spends.
		return fmt.Errorf("seat %d has already placed its stake at %s", f.Seat, held.outpoint)
	}
	t.funded[f.Seat] = out
	r.mu.Unlock()
	r.keep(t)
	return nil
}

// announceBonded tells the table where this seat's table bond landed.
func (r *Runtime) announceBonded(ctx context.Context, match string) error {
	t, mine, err := r.ourSeatAt(match)
	if err != nil {
		return err
	}
	r.mu.Lock()
	out, have := t.tableBondFunded[mine]
	r.mu.Unlock()
	if !have || out.outpoint == "" {
		return fmt.Errorf("this seat's table bond is not on the chain yet")
	}
	session, _, err := r.seatKeys(t.form.Terms().SID)
	if err != nil {
		return err
	}
	b, err := membership.SignBonded(t.form.Terms(), mine, out.outpoint, session)
	if err != nil {
		return fmt.Errorf("sign where the bond is: %w", err)
	}
	return r.send(ctx, t, KindBonded, schema.BondedFrom(b))
}

// adoptBonded takes another seat's word for where its table bond is.
func (r *Runtime) adoptBonded(ctx context.Context, match string, body schema.Bonded) error {
	b, err := body.Into()
	if err != nil {
		return fmt.Errorf("read where a bond is: %w", err)
	}
	t, err := r.tableOf(match)
	if err != nil {
		return err
	}
	if err := r.seatSaidIt(t, b.Seat, b.Signer); err != nil {
		return err
	}
	if err := b.Verify(t.form.Terms()); err != nil {
		return fmt.Errorf("seat %d's bond: %w", b.Seat, err)
	}
	bond, err := r.tableBondOf(t, b.Seat)
	if err != nil {
		return err
	}
	out, err := r.outputPaying(ctx, b.Outpoint, bond.PkScriptHex)
	if err != nil {
		return fmt.Errorf("seat %d's bond: %w", b.Seat, err)
	}
	if want := int64(t.form.Terms().BondAtoms); out.atoms < want {
		return fmt.Errorf("seat %d's bond holds %d atoms, and this table asks %d",
			b.Seat, out.atoms, want)
	}

	r.mu.Lock()
	if t.tableBondFunded == nil {
		t.tableBondFunded = map[uint32]staked{}
	}
	held, seen := t.tableBondFunded[b.Seat]
	if seen && held.outpoint != "" && held.outpoint != b.Outpoint {
		r.mu.Unlock()
		return fmt.Errorf("seat %d has already bonded at %s", b.Seat, held.outpoint)
	}
	t.tableBondFunded[b.Seat] = out
	r.mu.Unlock()
	r.keep(t)
	return nil
}

// announcePayout tells the table where this seat wants to be paid.
//
// Every seat announces, because the payout scripts all go into the one
// settlement everybody signs, and a seat whose script nobody has cannot be
// paid by it.
func (r *Runtime) announcePayout(ctx context.Context, match string) error {
	t, mine, err := r.ourSeatAt(match)
	if err != nil {
		return err
	}
	// The operator's address, which is the one place this game is allowed
	// to be paid. A game that could name its own would be naming itself.
	addr := r.Payout()
	if strings.TrimSpace(addr) == "" {
		return fmt.Errorf("no payout address has been set, so this seat cannot say where to be paid")
	}
	script, err := payScriptFor(addr, r.params)
	if err != nil {
		return err
	}
	session, _, err := r.seatKeys(t.form.Terms().SID)
	if err != nil {
		return err
	}
	p, err := membership.SignPayout(t.form.Terms(), mine, addr, session)
	if err != nil {
		return fmt.Errorf("sign the payout: %w", err)
	}
	// Kept before it is sent: a settlement built here has to pay this seat
	// the same script the others were told about.
	r.mu.Lock()
	if t.payouts == nil {
		t.payouts = map[uint32][]byte{}
	}
	t.payouts[mine] = script
	r.mu.Unlock()
	r.keep(t)
	return r.send(ctx, t, KindPayout, schema.PayoutFrom(p))
}

// adoptPayout takes another seat's payout script.
//
// Not checked against the chain, because it names nothing that exists yet - it
// is where money will go, not where money is. What is checked is who said it:
// a payout accepted from anybody would let a bystander redirect the pot.
func (r *Runtime) adoptPayout(_ context.Context, match string, body schema.Payout) error {
	p, err := body.Into()
	if err != nil {
		return fmt.Errorf("read a payout: %w", err)
	}
	t, err := r.tableOf(match)
	if err != nil {
		return err
	}
	if err := r.seatSaidIt(t, p.Seat, p.Signer); err != nil {
		return err
	}
	if err := p.Verify(t.form.Terms()); err != nil {
		return fmt.Errorf("seat %d's payout: %w", p.Seat, err)
	}
	// An address rather than a script on the wire, because an address is
	// checkable: one for the wrong chain, or one that is not an address at
	// all, is refused here rather than at settlement.
	script, err := payScriptFor(p.Address, r.params)
	if err != nil {
		return fmt.Errorf("seat %d's payout: %w", p.Seat, err)
	}

	r.mu.Lock()
	if t.payouts == nil {
		t.payouts = map[uint32][]byte{}
	}
	held, seen := t.payouts[p.Seat]
	if seen && len(held) > 0 && !bytesEqual(held, script) {
		r.mu.Unlock()
		// A settlement may already have been signed over the first one.
		return fmt.Errorf("seat %d has already said where to be paid", p.Seat)
	}
	t.payouts[p.Seat] = script
	r.mu.Unlock()
	r.keep(t)
	return nil
}

// seatSaidIt checks that the seat named is the seat that signed.
//
// Both halves matter. Without the seat check a member could announce on
// another seat's behalf; without the key check anybody at all could.
func (r *Runtime) seatSaidIt(t *table, seat uint32, signer []byte) error {
	seats, ok := t.form.Seats()
	if !ok {
		return fmt.Errorf("this table has no seating yet")
	}
	key, at := seats[seat]
	if !at {
		return fmt.Errorf("an announcement names seat %d, which is not at this table", seat)
	}
	if !bytesEqual(key, signer) {
		return fmt.Errorf("an announcement for seat %d was signed by somebody else", seat)
	}
	return nil
}

// outputPaying finds an outpoint and refuses it unless it pays the script this
// table derived.
func (r *Runtime) outputPaying(ctx context.Context, outpoint, pkScript string) (staked, error) {
	txid, vout, err := splitOutpoint(outpoint)
	if err != nil {
		return staked{}, err
	}
	out, err := r.bridge.UnconfirmedOutpoint(ctx, txid, vout)
	if err != nil {
		return staked{}, err
	}
	if !out.Found {
		return staked{}, fmt.Errorf("%s holds no coin", outpoint)
	}
	if !strings.EqualFold(out.PkScriptHex, pkScript) {
		return staked{}, fmt.Errorf(
			"%s does not pay the script this table derived; nothing was recorded", outpoint)
	}
	return staked{outpoint: outpoint, atoms: out.ValueAtoms}, nil
}

// tableOf finds a table that has joined.
//
// A table exists from the moment its invitation is accepted, but a game that
// posts one bond per table has nothing to join with until that bond is on the
// chain - so for a while there is a table with no formation behind it. Nothing
// downstream is written to expect one, so it is refused here rather than
// guarded for in a hundred places.
func (r *Runtime) tableOf(match string) (*table, error) {
	t, err := r.rawTable(match)
	if err != nil {
		return nil, err
	}
	if t.form == nil {
		return nil, fmt.Errorf("table %q has not joined yet; its seat bond is still being paid", match)
	}
	return t, nil
}

// rawTable finds a table whether or not it has joined. For the stages that run
// before there is a formation.
func (r *Runtime) rawTable(match string) (*table, error) {
	match = strings.ToLower(strings.TrimSpace(match))
	r.mu.Lock()
	t, ok := r.tables[match]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no table %q", match)
	}
	return t, nil
}

func bytesEqual(a, b []byte) bool {
	return hex.EncodeToString(a) == hex.EncodeToString(b)
}

// Tick moves every table on to a new chain height.
//
// Two things happen here, and both are the answer to the same fault: a message
// said once on a lossy channel is a message that may never arrive.
//
// A stake is announced the moment it is paid, which is seconds after the
// broadcast, when every peer still refuses it for want of a confirmation. Said
// once, that is a message guaranteed to be refused and never repeated - the
// table sits unfunded with the money already spent. So it is said again each
// block, and only while somebody is still missing: a peer that already has it
// ignores the repeat, so saying it again costs one small message and not
// saying it costs a table that never funds.
//
// And a deadline that has passed is a fact about the chain, so the formation is
// moved on when the height says so rather than when a message happens to
// arrive.
func (r *Runtime) Tick(ctx context.Context, height int64) {
	if height <= 0 {
		return
	}
	r.mu.Lock()
	tables := make([]*table, 0, len(r.tables))
	for _, t := range r.tables {
		tables = append(tables, t)
	}
	r.mu.Unlock()

	for _, t := range tables {
		if t.form == nil {
			// Still paying for its seat. Nothing to advance and
			// nothing to repeat: it has said nothing yet.
			continue
		}
		r.tickTable(ctx, t, height)
	}
}

// tickTable moves one table on.
func (r *Runtime) tickTable(ctx context.Context, t *table, height int64) {
	before := t.form.State()
	if t.form.Terms().Until > 0 && height > int64(t.form.Terms().Until) && !t.form.WindowClosed() {
		// The admission deadline is a height every peer can check, which
		// is why it is a height: a table that closed when each machine's
		// clock said so would seat different memberships.
		t.form.CloseWindow()
	}
	if t.form.State() != before {
		r.keep(t)
		r.log.Infof("table %s: %s", t.match, t.form.State())
	}
	if err := r.seatIfReady(ctx, t.match); err != nil {
		r.log.Debugf("table %s: not seated yet: %v", t.match, err)
	}

	r.mu.Lock()
	said := t.saidAt
	t.saidAt = height
	r.mu.Unlock()
	if height <= said {
		// Once a block, not once a poll.
		return
	}
	r.repeatAnnouncements(ctx, t)
}

// repeatFormation says again what this peer holds, and asks for what it does
// not, while a table is still forming.
//
// Both halves, because they heal different directions. A roster this peer
// published once, when it learned its last join, is the only thing that can
// teach a peer which never received that join - it cannot ask for what it does
// not know exists, so the roster has to be volunteered. A commit is asked for
// by name, so for that direction the ask is enough.
//
// Volunteering is safe to repeat: a roster carries every join with its
// signature, adopting one is idempotent, and a peer taught nothing publishes
// nothing back, so the repeat cannot echo.
func (r *Runtime) repeatFormation(ctx context.Context, t *table) {
	switch t.form.State() {
	case membership.Joining:
		if t.form.WindowClosed() {
			return
		}
		// A join is dropped by any peer that has not accepted the
		// invitation yet: it names a table they are not at. Two peers
		// joining seconds apart each drop the other's, and if a join is
		// published only once they both sit holding one until the
		// deadline ends a table they both wanted.
		//
		// Asking is enough to heal that, and asking is what happens: an
		// ask names the joins this peer holds, so the other side
		// answers with the one that went missing. Saying our own join
		// again as well would be a second message every block for a
		// round of latency.
		r.say(ctx, t, "what we are missing", r.publishResync)
	case membership.Formed, membership.Committed:
		r.say(ctx, t, "what we hold", r.publishRoster)
		r.say(ctx, t, "what we are missing", r.publishResync)
	}
}

// repeatAnnouncements says again what this seat has already said, while
// anybody is still short of it.
func (r *Runtime) repeatAnnouncements(ctx context.Context, t *table) {
	seats, ok := t.form.Seats()
	if !ok {
		r.repeatFormation(ctx, t)
		return
	}
	mine, ok := t.form.OurSeat()
	if !ok {
		return
	}
	r.mu.Lock()
	fundedAll := len(t.funded) == len(seats)
	bondedAll := len(t.tableBondFunded) == len(seats)
	paidAll := len(t.payouts) == len(seats)
	haveStake := t.funded[mine].outpoint != ""
	haveBond := t.tableBondFunded[mine].outpoint != ""
	r.mu.Unlock()

	if haveStake && !fundedAll {
		if err := r.announceFunded(ctx, t.match); err != nil {
			r.log.Debugf("table %s: repeating where the stake is: %v", t.match, err)
		}
	}
	if haveBond && !bondedAll {
		if err := r.announceBonded(ctx, t.match); err != nil {
			r.log.Debugf("table %s: repeating where the bond is: %v", t.match, err)
		}
	}
	if !paidAll && r.Payout() != "" {
		if err := r.announcePayout(ctx, t.match); err != nil {
			r.log.Debugf("table %s: repeating the payout: %v", t.match, err)
		}
	}
}
