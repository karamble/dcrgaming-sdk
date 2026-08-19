package runtime

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/wire"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

// authorized reports whether a sender may allocate state for a table.
//
// The sender is deliberately unused, and that is not an oversight - it is the
// model dcrpoker settled on and the reason is worth stating. This is not an
// identity check and cannot be one: the router is handed a Bison Relay uid,
// while a table's roster is session public keys, and a join carries no uid on
// purpose (a uid inside a relayed join would be an unverifiable claim about a
// third party). Nothing here could match the two up.
//
// What this is for is bounding memory. It runs before the assembler allocates
// anything for a sender, so its job is to stop a stranger from making this game
// hold reassembly state for tables it is not at. Whether a message is genuine is
// settled afterwards and properly, by the signature on the message itself.
//
// dcrbattleships returns true here, which is the same check with the bound taken
// off.
func (r *Runtime) authorized(sid, _ string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.tables[sid]
	return ok
}

// acceptInvite joins the table an invitation names.
//
// Accepting is a person's decision, taken in the dashboard where the invitation
// arrived; by the time this runs it has been made. What is left is deciding
// whether the table it describes is one this game will sit at.
func (r *Runtime) acceptInvite(ctx context.Context, req *gamingpb.AcceptInvite) (string, error) {
	gcid := strings.ToLower(strings.TrimSpace(req.GetGcid()))
	if !gcID.MatchString(gcid) {
		return "", fmt.Errorf("a group chat id is 64 hex characters")
	}
	inv, err := schema.ParseInvite(strings.TrimSpace(req.GetInvite()))
	if err != nil {
		return "", fmt.Errorf("read the invitation: %w", err)
	}
	if inv.Kind != schema.InviteKindTable {
		return "", fmt.Errorf("that is an invitation to %q, not a table", inv.Kind)
	}
	id := r.rules.Identity()
	if inv.Game != id.GameID {
		return "", fmt.Errorf("that invitation is for %q and this is %q", inv.Game, id.GameID)
	}
	if strings.TrimSpace(inv.SID) == "" {
		return "", fmt.Errorf("that invitation names no session")
	}

	terms, err := r.termsFor(inv)
	if err != nil {
		return "", err
	}
	creds, err := r.seatCredentials(terms)
	if err != nil {
		return "", err
	}
	form, err := membership.NewFormation(terms, creds)
	if err != nil {
		return "", fmt.Errorf("form the table: %w", err)
	}

	r.mu.Lock()
	if _, seen := r.tables[inv.SID]; seen {
		r.mu.Unlock()
		// Accepting twice is not an error. An operator pressing the
		// button again, or a retried request, gets the same answer.
		return inv.SID, nil
	}
	r.tables[inv.SID] = &table{match: inv.SID, gcID: gcid, form: form}
	r.mu.Unlock()

	if err := r.publishJoin(ctx, inv.SID); err != nil {
		return "", err
	}
	return inv.SID, nil
}

// termsFor composes the table's terms from the invitation and the game.
//
// The invitation decides everything it states, and the game fills in only what
// it left out. That split matters: an invitation is ordinary chat text, so
// whoever forwards it could hand one player one buy-in and another a different
// one, and winner-take-all divides a pot fairly only across equal stakes. Two
// players who read different invitations must fail to form a table rather than
// form one and discover it at settlement. Letting the game override a stated
// field would be exactly that failure, so a game that disagrees refuses instead.
//
// What the invitation cannot state is the bond, because schema.Invite has no
// field for one. Those come from the game, and both seats reach the same values
// by running the same game.
func (r *Runtime) termsFor(inv schema.Invite) (membership.Terms, error) {
	want, err := r.rules.Terms(inv.SID)
	if err != nil {
		return membership.Terms{}, fmt.Errorf("this game will not sit at that table: %w", err)
	}
	for _, c := range []struct {
		what    string
		invited uint64
		game    uint64
		into    func()
	}{
		{"buy-in", inv.BuyInAtoms, want.BuyInAtoms, func() { want.BuyInAtoms = inv.BuyInAtoms }},
		{"seat count", uint64(inv.Seats), uint64(want.Seats), func() { want.Seats = inv.Seats }},
		{"refund timelock", uint64(inv.CSVBlocks), uint64(want.CSVBlocks), func() { want.CSVBlocks = inv.CSVBlocks }},
		{"admission deadline", uint64(inv.Until), uint64(want.Until), func() { want.Until = inv.Until }},
	} {
		if c.invited == 0 {
			continue // the invitation states nothing; the game decides
		}
		if c.game != 0 && c.game != c.invited {
			return membership.Terms{}, fmt.Errorf(
				"the invitation states a %s of %d and this game plays %d",
				c.what, c.invited, c.game)
		}
		c.into()
	}
	want.Game, want.SID = inv.Game, inv.SID
	if err := want.Validate(); err != nil {
		return membership.Terms{}, fmt.Errorf("that invitation states no table this game can sit at: %w", err)
	}
	return want, nil
}

// seatCredentials derives this seat's keys and attaches the bond its join will
// bind to.
//
// A join names the seat's bond deposit, so the bond has to be on chain before a
// table can be joined at all. That ordering is the mechanism rather than an
// inconvenience: a seat whose join did not name a bond would be a seat with
// nothing to forfeit, which is the whole thing bonds are for.
func (r *Runtime) seatCredentials(terms membership.Terms) (membership.Credentials, error) {
	session, logKey, err := r.seatKeys(terms.SID)
	if err != nil {
		return membership.Credentials{}, err
	}
	// The bond key is derived WITHOUT the session id, and that is not an
	// oversight. identity.BondDeposit is one outpoint per identity, so the
	// key that opens it has to be one key per identity too. Deriving it per
	// table would build a script the stored deposit was never paid into, and
	// the join would bind to a bond nobody could spend.
	bond, err := r.identity.DeriveKey(r.seatTags.Bond, "")
	if err != nil {
		return membership.Credentials{}, fmt.Errorf("derive this seat's bond key: %w", err)
	}
	creds := membership.Credentials{Session: session, Log: logKey, Bond: bond}
	outpoint := r.identity.BondDeposit()
	if strings.TrimSpace(outpoint) == "" {
		return membership.Credentials{}, fmt.Errorf(
			"this seat has no bond deposit yet, so it has nothing to stake against its word; " +
				"fund one before joining a table")
	}
	lock := terms.BondLockBlocks
	if lock == 0 {
		// A table that states no bond terms uses the escrow floor, which
		// is what dcrpoker has always done.
		lock = escrow.MinBondBlocks
	}
	script, err := escrow.BondScript(creds.Bond.PubKey().SerializeCompressed(), lock)
	if err != nil {
		return membership.Credentials{}, fmt.Errorf("build this seat's bond script: %w", err)
	}
	creds.BondOutpoint, creds.BondScript = outpoint, script
	return creds, nil
}

// seatKeys derives the two per-table keys. The bond is not among them; see
// seatCredentials.
func (r *Runtime) seatKeys(sid string) (session, logKey *secp256k1.PrivateKey, err error) {
	if session, err = r.identity.DeriveKey(r.seatTags.Session, sid); err != nil {
		return nil, nil, fmt.Errorf("derive this seat's session key: %w", err)
	}
	if logKey, err = r.identity.DeriveKey(r.seatTags.Log, sid); err != nil {
		return nil, nil, fmt.Errorf("derive this seat's log key: %w", err)
	}
	return session, logKey, nil
}

// publishJoin announces this seat's claim to the table.
func (r *Runtime) publishJoin(ctx context.Context, match string) error {
	r.mu.Lock()
	t, ok := r.tables[match]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("no table %q", match)
	}
	join := t.form.Ours()
	if join == nil {
		return fmt.Errorf("this seat has no join to publish")
	}
	return r.send(ctx, t, schema.KindJoin, join)
}

// send puts one message to a table's group chat.
func (r *Runtime) send(ctx context.Context, t *table, kind schema.Kind, body any) error {
	// ClassForm, because formation traffic has to outlive a relay backlog:
	// a join queued behind one and expiring in transit forms one table and
	// aborts the other.
	return r.router.Send(ctx, t.gCID(), t.match, t.match, kind, body, wire.ClassForm)
}

func (t *table) gCID() string { return t.gcID }

// addJoin takes another seat's claim to the table.
func (r *Runtime) addJoin(match string, j *membership.Join) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tables[match]
	if !ok {
		return fmt.Errorf("a join arrived for a table this game is not at")
	}
	return t.form.AddJoin(j)
}

// addCommit takes another seat's commitment to the roster.
func (r *Runtime) addCommit(match string, c *membership.Commit) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tables[match]
	if !ok {
		return fmt.Errorf("a commit arrived for a table this game is not at")
	}
	return t.form.AddCommit(c)
}

// seatIfReady sets the beacon once the chain has reached its height, which is
// what turns an agreed roster into seats.
//
// The beacon is a block hash and nothing anybody chose, so no seat can steer
// who sits where. Calling this before the height, or twice, does nothing.
func (r *Runtime) seatIfReady(ctx context.Context, match string) error {
	r.mu.Lock()
	t, ok := r.tables[match]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("no table %q", match)
	}
	if t.form.Seated() {
		return nil
	}
	if !t.form.WindowClosed() {
		t.form.CloseWindow()
	}
	if !t.form.Agreed() {
		return nil
	}

	want := t.form.BeaconHeight()
	tip, err := r.bridge.ChainTip(ctx)
	if err != nil {
		return err
	}
	if tip.Height < int64(want) {
		return nil
	}
	hash, err := r.bridge.BlockHash(ctx, want)
	if err != nil {
		return err
	}
	raw, err := hex.DecodeString(hash)
	if err != nil {
		return fmt.Errorf("the bridge gave a block hash that is not hex: %w", err)
	}
	if err := t.form.SetBeacon(raw); err != nil {
		return err
	}

	seats, _ := t.form.Seats()
	r.mu.Lock()
	t.seats = seats
	r.mu.Unlock()

	if h, ok := r.rules.(Seated); ok {
		h.Seated(ctx, match, seats)
	}
	return nil
}
