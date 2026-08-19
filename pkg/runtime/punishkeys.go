package runtime

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"strings"

	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

// KindPunishKey carries a punishment-key announcement.
//
// The runtime's own, like join, commit and settle: a game that used this name
// for its own traffic would find those messages disappearing into the runtime.
const KindPunishKey schema.Kind = "punishkey"

// announcePunishKey tells the table which key this seat's opponent will be able
// to punish it with.
//
// Every seat has to announce before any forfeitable bond can be built, because
// a bond names the key that can take it. Announced rather than derived by the
// other side because only the holder can prove possession, and a bond naming a
// key nobody holds is a bond nobody can ever punish.
//
// Heads-up only. The punishment key is derived against one opponent, and the
// ladder that spends these bonds is written for two seats.
func (r *Runtime) announcePunishKey(ctx context.Context, match string) error {
	r.mu.Lock()
	t, ok := r.tables[match]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("no table %q", match)
	}
	if len(r.punishTag) == 0 {
		return fmt.Errorf("this game states no punishment-key domain tag, so it cannot announce one")
	}
	seats, ok := t.form.Seats()
	if !ok {
		return fmt.Errorf("this table has no seating yet")
	}
	if len(seats) != 2 {
		return fmt.Errorf("forfeitable bonds are heads-up, and this table seats %d", len(seats))
	}
	mine, ok := t.form.OurSeat()
	if !ok {
		return fmt.Errorf("this table has not seated us")
	}
	matchID, ok := t.form.RosterHash()
	if !ok {
		return fmt.Errorf("this table has no settled roster")
	}

	punish, err := r.punishKeyFor(t, seats, mine)
	if err != nil {
		return err
	}
	session, _, err := r.seatKeys(t.form.Terms().SID)
	if err != nil {
		return err
	}
	note, err := membership.SignPunishNote(r.punishTag, t.form.Terms(), matchID, mine, session, punish)
	if err != nil {
		return fmt.Errorf("sign the announcement: %w", err)
	}

	r.mu.Lock()
	if t.punishPubs == nil {
		t.punishPubs = map[uint32][]byte{}
	}
	t.punishPubs[mine] = note.Pub
	t.punish = punish
	r.mu.Unlock()

	return r.send(ctx, t, KindPunishKey, note)
}

// punishKeyFor derives the key this seat will punish its opponent with.
func (r *Runtime) punishKeyFor(t *table, seats map[uint32][]byte, mine uint32) (*secp256k1.PrivateKey, error) {
	matchID, ok := t.form.RosterHash()
	if !ok {
		return nil, fmt.Errorf("this table has no settled roster")
	}
	var opp []byte
	for seat, key := range seats {
		if seat != mine {
			opp = key
		}
	}
	if len(opp) == 0 {
		return nil, fmt.Errorf("this table has no opponent to punish")
	}
	return forfeit.PunishmentKeyFrom(r.identity.Seed(), hex.EncodeToString(matchID[:]), opp)
}

// adoptPunishKey takes another seat's announcement.
//
// Verified before it is kept, and both signatures matter: without the session
// one anybody could announce on a seat's behalf, and without the proof of
// possession a seat could name a key it does not hold and hand its opponent a
// bond that can never be punished.
func (r *Runtime) adoptPunishKey(ctx context.Context, match string, n membership.PunishNote) error {
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
	sessionPub, ok := seats[n.Seat]
	if !ok {
		return fmt.Errorf("an announcement names seat %d, which is not at this table", n.Seat)
	}
	matchID, ok := t.form.RosterHash()
	if !ok {
		return fmt.Errorf("this table has no settled roster")
	}
	if err := membership.VerifyPunishNote(r.punishTag, t.form.Terms(), matchID, n, sessionPub); err != nil {
		return fmt.Errorf("seat %d's punishment key: %w", n.Seat, err)
	}

	r.mu.Lock()
	if t.punishPubs == nil {
		t.punishPubs = map[uint32][]byte{}
	}
	if held, seen := t.punishPubs[n.Seat]; seen && hex.EncodeToString(held) != hex.EncodeToString(n.Pub) {
		r.mu.Unlock()
		// Two different keys from one seat is not a mistake to reconcile.
		// Whichever bond was built first, the other announcement would
		// change what can punish it.
		return fmt.Errorf("seat %d has already announced a different punishment key", n.Seat)
	}
	t.punishPubs[n.Seat] = n.Pub
	ready := len(t.punishPubs) == len(seats)
	r.mu.Unlock()

	if ready {
		return r.buildForfeitableBonds(t)
	}
	return nil
}

// buildForfeitableBonds derives every seat's forfeitable bond, once every seat
// has announced the key that can take it.
func (r *Runtime) buildForfeitableBonds(t *table) error {
	seats, ok := t.form.Seats()
	if !ok {
		return fmt.Errorf("this table has no seating yet")
	}
	logs, ok := t.form.LogSeats()
	if !ok {
		return fmt.Errorf("this table has no log keys")
	}
	matchID, ok := t.form.RosterHash()
	if !ok {
		return fmt.Errorf("this table has no settled roster")
	}
	lock := t.form.Terms().BondLockBlocks
	if lock == 0 {
		return fmt.Errorf("this table states no bond lock, so it can have no forfeitable bonds")
	}

	r.mu.Lock()
	pubs := make(map[uint32][]byte, len(t.punishPubs))
	for k, v := range t.punishPubs {
		pubs[k] = v
	}
	r.mu.Unlock()

	bonds, err := membership.ForfeitableBonds(
		hex.EncodeToString(matchID[:]), seats, logs, pubs, lock, r.params)
	if err != nil {
		return fmt.Errorf("derive the forfeitable bonds: %w", err)
	}
	r.mu.Lock()
	t.forfeitBonds = map[uint32]membership.ForfeitableBond{}
	for _, b := range bonds {
		t.forfeitBonds[b.Seat] = b
	}
	r.mu.Unlock()
	return nil
}

// ForfeitableBond reports a seat's forfeitable bond, once every seat has
// announced.
func (r *Runtime) ForfeitableBond(match string, seat uint32) (membership.ForfeitableBond, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tables[match]
	if !ok {
		return membership.ForfeitableBond{}, false
	}
	b, has := t.forfeitBonds[seat]
	return b, has
}

// FundForfeitBond pays this seat's forfeitable bond, which is the money its
// opponent takes if it equivocates.
//
// Separate from Fund because it is a different bond with a different purpose:
// a stake is what a seat plays for, and this is what it stakes against its own
// honesty. A table can have one without the other.
func (r *Runtime) FundForfeitBond(ctx context.Context, match string) error {
	r.mu.Lock()
	t, ok := r.tables[match]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("no table %q", match)
	}
	mine, ok := t.form.OurSeat()
	if !ok {
		return fmt.Errorf("this table has not seated us")
	}
	bond, ok := r.ForfeitableBond(match, mine)
	if !ok {
		return fmt.Errorf("no forfeitable bond for seat %d yet; every seat has to announce first", mine)
	}
	r.mu.Lock()
	already, funded := t.forfeitFunded[mine]
	r.mu.Unlock()
	if funded && already.outpoint != "" {
		return nil
	}

	atoms := int64(t.form.Terms().BondAtoms)
	if atoms <= 0 {
		return fmt.Errorf("this table states no bond amount")
	}
	rec, err := r.askFor(ctx, spend.Record{
		Match: match, Seat: mine, Purpose: "forfeitbond",
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
	if t.forfeitFunded == nil {
		t.forfeitFunded = map[uint32]staked{}
	}
	t.forfeitFunded[mine] = out
	r.mu.Unlock()
	return nil
}

// NoteForfeitBond records where another seat's forfeitable bond landed, which
// is what a sweep spends.
//
// Told rather than discovered because the bond is somebody else's payment and
// this peer never saw it go out. The script is checked against the one this
// peer derived, so a seat cannot point the sweep at an output it controls.
func (r *Runtime) NoteForfeitBond(ctx context.Context, match string, seat uint32, outpoint string) error {
	bond, ok := r.ForfeitableBond(match, seat)
	if !ok {
		return fmt.Errorf("no forfeitable bond for seat %d yet", seat)
	}
	txid, vout, err := splitOutpoint(outpoint)
	if err != nil {
		return err
	}
	out, err := r.bridge.UnconfirmedOutpoint(ctx, txid, vout)
	if err != nil {
		return err
	}
	if !out.Found {
		return fmt.Errorf("%s holds no coin", outpoint)
	}
	if !strings.EqualFold(out.PkScriptHex, bond.PkScriptHex) {
		return fmt.Errorf("%s does not pay seat %d's forfeitable bond; "+
			"nothing was recorded and no sweep will point at it", outpoint, seat)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tables[match]
	if t == nil {
		return fmt.Errorf("no table %q", match)
	}
	if t.forfeitFunded == nil {
		t.forfeitFunded = map[uint32]staked{}
	}
	t.forfeitFunded[seat] = staked{outpoint: outpoint, atoms: out.ValueAtoms}
	return nil
}
