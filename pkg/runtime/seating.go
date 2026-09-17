package runtime

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

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
// AcceptInvite joins the table an invitation names, in the group chat it
// arrived in, and returns the session it joined.
//
// The bridge's own AcceptInvite request lands here too. It is exported because
// accepting is a decision, and a game that takes that decision somewhere other
// than the operator's console - a test, a bot, a lobby of its own - would
// otherwise have no way to act on it.
func (r *Runtime) AcceptInvite(ctx context.Context, link, gcid string) (string, error) {
	return r.acceptInvite(ctx, &gamingpb.AcceptInvite{Invite: link, Gcid: gcid})
}

func (r *Runtime) acceptInvite(ctx context.Context, req *gamingpb.AcceptInvite) (string, error) {
	r.admissionMu.Lock()
	defer r.admissionMu.Unlock()
	if err := r.healthy(); err != nil {
		return "", err
	}
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

	terms, err := r.resolveInvite(ctx, inv)
	if err != nil {
		return "", err
	}

	r.mu.Lock()
	if why, over := r.ended[inv.SID]; over {
		r.mu.Unlock()
		return "", fmt.Errorf("this session already ended: %s", why)
	}
	if old, seen := r.tables[inv.SID]; seen {
		r.mu.Unlock()
		if old.terms != terms || old.gcID != gcid {
			return "", fmt.Errorf("session already accepted with different terms or group chat")
		}
		// Accepting twice is not an error. An operator pressing the
		// button again, or a retried request, gets the same answer.
		return inv.SID, nil
	}
	r.mu.Unlock()
	tip, err := r.bridge.ChainTip(ctx)
	if err != nil {
		return "", err
	}
	if tip.Height > int64(terms.Until) {
		return "", fmt.Errorf("admission deadline passed")
	}
	t := &table{match: inv.SID, gcID: gcid, terms: terms}
	{
		authority, err := r.bridge.FinancialAuthority(ctx, inv.SID)
		if err != nil {
			return "", err
		}
		pub := authority.GetPublicKey()
		t.bridgePayout = authority.GetPayoutAddress()
		if _, err := payScriptFor(t.bridgePayout, r.params); err != nil {
			return "", fmt.Errorf("invalid bridge payout destination: %w", err)
		}
		key, err := hex.DecodeString(pub)
		if err != nil || len(key) != 33 {
			return "", fmt.Errorf("bridge returned an invalid spending key")
		}
		if _, err = secp256k1.ParsePubKey(key); err != nil {
			return "", err
		}
		t.bridgeKey = pub
	}
	if err := r.keep(t); err != nil {
		return "", err
	}
	r.mu.Lock()
	r.tables[inv.SID] = t
	r.mu.Unlock()

	// Register before approval so the game can display pending seating.
	r.startAdmission(t)
	return inv.SID, nil
}

// joinWhenBonded pays this table's seat bond and then joins with it.
func (r *Runtime) joinWhenBonded(ctx context.Context, t *table) {
	tip, err := r.bridge.ChainTip(ctx)
	if err != nil {
		r.log.Errorf("table %s: checking admission deadline: %v", t.match, err)
		return
	}
	r.mu.Lock()
	alreadyPaid := t.seatBond.outpoint != ""
	r.mu.Unlock()
	if tip.Height > int64(t.terms.Until) && !alreadyPaid {
		r.mu.Lock()
		t.recoveryOnly = true
		t.recoveryReason = "admission expired"
		r.mu.Unlock()
		if err = r.keep(t); err != nil {
			r.log.Errorf("table %s: retaining expired admission: %v", t.match, err)
		}
		return
	}
	if err := r.FundSeatBond(ctx, t.match); err != nil {
		r.log.Errorf("table %s: the seat bond was not paid: %v", t.match, err)
		return
	}
	if err := r.formAndJoin(ctx, t); err != nil {
		r.log.Errorf("table %s: joining: %v", t.match, err)
	}
}

// formAndJoin builds this seat's credentials, forms the table and says so.
func (r *Runtime) formAndJoin(ctx context.Context, t *table) error {
	t.formMu.Lock()
	defer t.formMu.Unlock()
	r.mu.Lock()
	existing := t.formation() != nil
	r.mu.Unlock()
	if existing {
		return nil
	}
	tip, err := r.bridge.ChainTip(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	alreadyPaid := t.seatBond.outpoint != ""
	r.mu.Unlock()
	if tip.Height > int64(t.terms.Until) && !alreadyPaid {
		r.mu.Lock()
		t.recoveryOnly = true
		t.recoveryReason = "admission expired"
		r.mu.Unlock()
		if err := r.keep(t); err != nil {
			return err
		}
		return fmt.Errorf("admission expired; deposit retained for recovery")
	}

	creds, err := r.seatCredentials(t, t.terms)
	if err != nil {
		return err
	}
	form, err := membership.NewFormation(t.terms, creds)
	if err != nil {
		return fmt.Errorf("form the table: %w", err)
	}
	// The bridge has already located the exact bond output in the mempool or
	// chain. Publish the signed join now so a full table cannot expire merely
	// because its bonds are still gaining confirmations. Seating and every
	// later financial step remain blocked until all bonds are confirmed.
	if err = r.checkAdmissionBondAnnouncement(ctx, t.terms, form.Ours()); err != nil {
		return err
	}
	r.mu.Lock()
	t.setFormation(form)
	r.mu.Unlock()

	if err := r.keep(t); err != nil {
		return err
	}
	return r.publishJoin(ctx, t.match)
}

// termsFor copies the operator's advertised economics without substituting
// game defaults. A game may reject terms it does not support.
func (r *Runtime) termsFor(inv schema.Invite) (membership.Terms, error) {
	if inv.TableBondAtoms != 0 || inv.TableBondBlocks != 0 {
		return membership.Terms{}, fmt.Errorf("additional table bonds are not supported by this runtime")
	}
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
		{"admission bond", inv.AdmissionAtoms, want.BondAtoms, func() { want.BondAtoms = inv.AdmissionAtoms }},
		{"admission bond delay", uint64(inv.AdmissionBlocks), uint64(want.BondLockBlocks), func() { want.BondLockBlocks = inv.AdmissionBlocks }},
		{"seat count", uint64(inv.Seats), uint64(want.Seats), func() { want.Seats = inv.Seats }},
		{"refund timelock", uint64(inv.CSVBlocks), uint64(want.CSVBlocks), func() { want.CSVBlocks = inv.CSVBlocks }},
		{"admission deadline", uint64(inv.Until), uint64(want.Until), func() { want.Until = inv.Until }},
	} {
		if c.invited == 0 {
			return membership.Terms{}, fmt.Errorf("invitation omits %s", c.what)
		}
		_, resolves := r.rules.(InviteResolver)
		if !resolves && c.game != 0 && c.game != c.invited {
			return membership.Terms{}, fmt.Errorf(
				"the invitation states a %s of %d and this game plays %d",
				c.what, c.invited, c.game)
		}
		c.into()
	}
	want.Game, want.SID = inv.Game, inv.SID
	if _, resolves := r.rules.(InviteResolver); resolves {
		return want, nil
	}
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
func (r *Runtime) seatCredentials(t *table, terms membership.Terms) (membership.Credentials, error) {
	session, logKey, err := r.seatKeys(terms.SID)
	if err != nil {
		return membership.Credentials{}, err
	}
	bond, err := r.seatBondKeyFor(terms.SID)
	if err != nil {
		return membership.Credentials{}, err
	}
	outpoint, err := r.seatBondOutpoint(t)
	if err != nil {
		return membership.Credentials{}, err
	}
	script, err := r.bondScriptFor(terms, t.bridgeKey)
	if err != nil {
		return membership.Credentials{}, err
	}
	return membership.Credentials{
		Session: session, Log: logKey, Bond: bond,
		BondOutpoint: outpoint, BondScript: script,
	}, nil
}

// seatKeys derives the two per-table keys. The bond is not among them; see
// seatCredentials.
func (r *Runtime) seatKeys(sid string) (session, logKey *secp256k1.PrivateKey, err error) {
	if err = r.healthy(); err != nil {
		return nil, nil, err
	}
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
	join := t.formation().Ours()
	if join == nil {
		return fmt.Errorf("this seat has no join to publish")
	}
	return r.send(ctx, t, schema.KindJoin, schema.JoinFrom(join))
}

// send puts one message to a table's group chat.
func (r *Runtime) send(ctx context.Context, t *table, kind schema.Kind, body any) error {
	r.mu.Lock()
	recovery := t.recoveryOnly
	r.mu.Unlock()
	if recovery {
		return fmt.Errorf("table is retained for recovery only")
	}
	if err := r.healthy(); err != nil {
		return err
	}
	return r.router.Send(ctx, t.gCID(), t.match, t.match, kind, body, classOf(kind))
}

// classOf is how long one of the runtime's messages stays worth delivering.
//
// Formation traffic has to outlive a relay backlog. Everything else describes
// current financial state and uses its own shorter validity window. Nothing in
// this classification authorizes periodic republication.
func classOf(kind schema.Kind) wire.Class {
	switch kind {
	case schema.KindJoin, schema.KindCommit, KindRoster:
		return wire.ClassDurable
	case KindFunded, KindBonded, KindPayout:
		return wire.ClassDurable
	}
	return wire.ClassState
}

func (t *table) gCID() string { return t.gcID }

// addJoin takes another seat's claim to the table.
func (r *Runtime) addJoin(ctx context.Context, match string, j *membership.Join) error {
	t, err := r.tableOf(match)
	if err != nil {
		return fmt.Errorf("a join arrived for a table this game is not at")
	}
	from := at(t)
	if err = r.checkAdmissionBondAnnouncement(ctx, t.terms, j); err != nil {
		return fmt.Errorf("verify announced admission bond: %w", err)
	}
	r.mu.Lock()
	err = t.formation().AddJoin(j)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	// A join nobody will send twice decides the seating, so it is written
	// down as soon as it is taken. Outside the lock, because writing it
	// calls the game.
	r.keep(t)
	r.advance(ctx, t, from, true)
	return nil
}

// addCommit takes another seat's commitment to the roster.
func (r *Runtime) addCommit(ctx context.Context, match string, c *membership.Commit) error {
	t, err := r.tableOf(match)
	if err != nil {
		return fmt.Errorf("a commit arrived for a table this game is not at")
	}
	from := at(t)
	r.mu.Lock()
	err = t.formation().AddCommit(c)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	r.keep(t)
	r.advance(ctx, t, from, false)
	return nil
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
	if t.formation() == nil {
		return ErrNotSeated
	}
	if t.formation().Seated() {
		return nil
	}
	// Admission is not shut here. Closing it short of a full table aborts
	// the table, and this runs on every arriving message - so a table would
	// abort the moment the first join reached it, before the second player
	// had any chance to answer. The deadline shuts admission, in Tick, at a
	// height everybody reads the same way.
	if !t.formation().Agreed() {
		return nil
	}

	want := t.formation().BeaconHeight()
	tip, err := r.bridge.ChainTip(ctx)
	if err != nil {
		return err
	}
	if tip.Height < int64(want) {
		return nil
	}
	if err := r.CheckAdmissionBonds(ctx, match); err != nil {
		return fmt.Errorf("admission bonds are not confirmed: %w", err)
	}
	hash, err := r.bridge.BlockHash(ctx, want)
	if err != nil {
		return err
	}
	raw, err := hex.DecodeString(hash)
	if err != nil {
		return fmt.Errorf("the bridge gave a block hash that is not hex: %w", err)
	}
	if err := t.formation().SetBeacon(raw); err != nil {
		return err
	}

	seats, _ := t.formation().Seats()
	r.mu.Lock()
	t.seats = seats
	r.mu.Unlock()
	// The beacon is what turns an agreed roster into seats, so a restart
	// that lost it would seat this table differently from everyone else.
	r.keep(t)

	// Announce before the game is told, so a game that starts play on Seated
	// finds the exchange already under way.

	if h, ok := r.rules.(Seated); ok {
		h.Seated(ctx, match, seats)
	}
	return nil
}

// KindRoster carries what a peer says it holds. The runtime's own.
const KindRoster = schema.KindRoster

// publishRoster says what this peer holds, so the table can agree.
//
// This is what turns a pile of joins into a membership. Every peer signs a
// claim about the set it holds, and a table forms when everybody's claim says
// the same thing - which is why it is signed: an unsigned claim would let
// anybody manufacture agreement and drive peers holding different join sets to
// bind different memberships.
//
// Only a peer with a full table can claim one. Short of that there is nothing
// to assert, and saying so anyway would be claiming agreement with a set
// nobody holds. Seating is later and separate - it waits for the block it
// draws its order from - so an assertion must not wait on it, or no table
// would ever agree and every one would sit out its deadline.
func (r *Runtime) publishRoster(ctx context.Context, match string) error {
	t, err := r.tableOf(match)
	if err != nil {
		return err
	}
	var assertion *membership.Assertion
	if a, err := t.formation().Assertion(); err == nil {
		assertion = a
	}
	seats := map[uint32][]byte{}
	if s, ok := t.formation().Seats(); ok {
		seats = s
	}
	body := schema.RosterFrom(t.formation().Terms(), seats, t.formation().Joins(), assertion)
	return r.send(ctx, t, KindRoster, body)
}

// adoptRoster takes another peer's claim about what it holds.
//
// The joins are what make the claim checkable, and they are checked before any
// is kept: a member could otherwise get a key nobody joined with admitted by
// burying it among real ones.
func (r *Runtime) adoptRoster(ctx context.Context, match string, body schema.Roster) error {
	t, err := r.tableOf(match)
	if err != nil {
		return err
	}
	terms := t.formation().Terms()
	joins := make([]*membership.Join, 0, len(body.Joins))
	for i, wj := range body.Joins {
		j, err := wj.Into()
		if err != nil {
			return fmt.Errorf("roster join %d: %w", i, err)
		}
		if err := j.Verify(terms); err != nil {
			if body.Terms != nil && body.Terms.Into() != terms {
				// The likelier cause, and the more useful thing
				// to say: two peers read different invitations,
				// so neither is wrong about its own join.
				return fmt.Errorf(
					"that roster was computed under different terms; we read different invitations")
			}
			return fmt.Errorf("roster join %d: %w", i, err)
		}
		if err := r.checkAdmissionBondAnnouncement(ctx, terms, j); err != nil {
			return fmt.Errorf("roster join %d bond: %w", i, err)
		}
		joins = append(joins, j)
	}

	assertion, err := body.Assertion()
	if err != nil {
		return err
	}
	from := at(t)
	if assertion != nil {
		if err := t.formation().AddAssertion(assertion, joins); err != nil {
			return err
		}
	} else {
		// A roster with no claim is just a carrier for joins.
		for _, j := range joins {
			if err := t.formation().AddJoin(j); err != nil {
				return err
			}
		}
	}
	r.keep(t)

	// A roster is a one-shot assertion, never a request. Receiving one does not
	// echo it. If its signed joins complete our local set, that is a new local
	// Formed transition and publishes our own assertion exactly once.
	r.advance(ctx, t, from, true)
	return nil
}

// where is one table's position, taken before something is folded into it so
// advance can tell what changed.
type where struct {
	state membership.State
}

func at(t *table) where {
	return where{state: t.formation().State()}
}

// advance moves a table on from wherever it was, and says what that means to
// the rest of the table.
//
// Everything that folds something into a formation ends here. The formation
// itself never speaks; this publishes only a newly reached local transition.
func (r *Runtime) advance(ctx context.Context, t *table, from where, publishAssertion bool) {
	moved := t.formation().State() != from.state

	switch t.formation().State() {
	case membership.Joining:
		// A partial roster is not a state transition and is never broadcast.

	case membership.Formed:
		if publishAssertion && moved {
			r.say(ctx, t, "what we hold", r.publishRoster)
		}
		// Bind when everyone says they hold this same membership, or
		// when admission shuts, whichever comes first.
		//
		// The deadline is what makes "no more joins are coming" a fact,
		// and alone it would do - but it would also mean every table
		// takes as long as its window to form, which is a lobby nobody
		// watches. Unanimity is the fast path: it does not prove no
		// straggler exists, only the deadline does, but it does mean
		// every member has seen exactly this set. What is left is a
		// race that resolves to no game, never to two tables.
		if t.formation().Agreed() || t.formation().WindowClosed() {
			c, err := t.formation().Bind()
			if err != nil {
				r.log.Errorf("table %s: binding: %v", t.match, err)
				return
			}
			r.keep(t)
			if err := r.send(ctx, t, schema.KindCommit, schema.CommitFrom(c)); err != nil {
				r.log.Warnf("table %s: publishing our commit: %v", t.match, err)
			}
			// Binding may have finished the table on its own, if
			// everybody else's commit arrived first.
			r.advance(ctx, t, where{state: membership.Formed}, false)
		}

	case membership.Committed:
		// Receiving a commit never causes another roster announcement.

	case membership.Settled:
		if err := r.seatIfReady(ctx, t.match); err != nil {
			r.log.Debugf("table %s: not seated yet: %v", t.match, err)
		}

	case membership.Aborted:
		if moved {
			r.log.Warnf("table %s did not form: %s", t.match, t.formation().Reason())
			r.keep(t)
		}
	}
}

// say runs one of the publishing steps and logs rather than fails. Nothing
// here is worth unwinding a formation over; BR retains every successful send.
func (r *Runtime) say(ctx context.Context, t *table, what string, f func(context.Context, string) error) {
	if err := f(ctx, t.match); err != nil {
		r.log.Warnf("table %s: saying %s: %v", t.match, what, err)
	}
}
