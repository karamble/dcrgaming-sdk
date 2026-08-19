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
	if why, over := r.ended[inv.SID]; over {
		r.mu.Unlock()
		return "", fmt.Errorf("this session already ended: %s", why)
	}
	if _, seen := r.tables[inv.SID]; seen {
		r.mu.Unlock()
		// Accepting twice is not an error. An operator pressing the
		// button again, or a retried request, gets the same answer.
		return inv.SID, nil
	}
	t := &table{match: inv.SID, gcID: gcid, form: form}
	r.tables[inv.SID] = t
	r.mu.Unlock()

	r.keep(t)
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
	return r.send(ctx, t, schema.KindJoin, schema.JoinFrom(join))
}

// send puts one message to a table's group chat.
func (r *Runtime) send(ctx context.Context, t *table, kind schema.Kind, body any) error {
	return r.router.Send(ctx, t.gCID(), t.match, t.match, kind, body, classOf(kind))
}

// classOf is how long one of the runtime's messages stays worth delivering.
//
// Formation traffic has to outlive a relay backlog: a join queued behind one
// and expiring in transit forms one table and aborts the other, which is a
// thing that happened. Everything else is state sync - where the money went,
// where to pay it, a signature on the payout - and a stale one of those is
// worse than none, because it describes a table that has moved on. Each is
// repeated while it still matters, so a short life costs nothing.
func classOf(kind schema.Kind) wire.Class {
	switch kind {
	case schema.KindJoin, schema.KindCommit, KindRoster, KindResync, KindResyncReply:
		return wire.ClassForm
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
	r.mu.Lock()
	err = t.form.AddJoin(j)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	// A join nobody will send twice decides the seating, so it is written
	// down as soon as it is taken. Outside the lock, because writing it
	// calls the game.
	r.keep(t)
	r.advance(ctx, t, from)
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
	err = t.form.AddCommit(c)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	r.keep(t)
	r.advance(ctx, t, from)
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
	if t.form.Seated() {
		return nil
	}
	// Admission is not shut here. Closing it short of a full table aborts
	// the table, and this runs on every arriving message - so a table would
	// abort the moment the first join reached it, before the second player
	// had any chance to answer. The deadline shuts admission, in Tick, at a
	// height everybody reads the same way.
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
	// The beacon is what turns an agreed roster into seats, so a restart
	// that lost it would seat this table differently from everyone else.
	r.keep(t)

	// Announce before the game is told, so a game that starts play on Seated
	// finds the exchange already under way.
	if len(r.punishTag) > 0 {
		if err := r.announcePunishKey(ctx, match); err != nil {
			r.log.Warnf("table %s: announcing a punishment key: %v", match, err)
		}
	}
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
	if a, err := t.form.Assertion(); err == nil {
		assertion = a
	}
	seats := map[uint32][]byte{}
	if s, ok := t.form.Seats(); ok {
		seats = s
	}
	body := schema.RosterFrom(t.form.Terms(), seats, t.form.Joins(), assertion)
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
	terms := t.form.Terms()
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
		joins = append(joins, j)
	}

	assertion, err := body.Assertion()
	if err != nil {
		return err
	}
	from := at(t)
	agreed := t.form.Agreed()
	if assertion != nil {
		if err := t.form.AddAssertion(assertion, joins); err != nil {
			return err
		}
	} else {
		// A roster with no claim is just a carrier for joins.
		for _, j := range joins {
			if err := t.form.AddJoin(j); err != nil {
				return err
			}
		}
	}
	r.keep(t)

	// Answered, but only when it told us something. A peer whose own join
	// went astray learns the table from somebody else's roster and has
	// never said what it holds - without an answer here it never would, and
	// the table would wait out its deadline one assertion short.
	//
	// Agreement counts as news even when the join set did not move, because
	// it is the thing the other side is waiting to hear. A roster that
	// changes neither is not answered, so two peers that already agree fall
	// silent instead of answering each other forever.
	if t.form.Agreed() != agreed {
		r.say(ctx, t, "what we hold", r.publishRoster)
	}
	r.advance(ctx, t, from)
	return nil
}

// KindResync asks the table for what this peer is missing, and KindResyncReply
// answers. The runtime's own.
const (
	KindResync      = schema.KindResync
	KindResyncReply = schema.KindResyncReply
)

// where is one table's position, taken before something is folded into it so
// advance can tell what changed.
type where struct {
	state membership.State
	joins int
}

func at(t *table) where {
	return where{state: t.form.State(), joins: len(t.form.Joins())}
}

// advance moves a table on from wherever it was, and says what that means to
// the rest of the table.
//
// Everything that folds something into a formation ends here, because the
// formation itself never speaks: it takes joins, commits and assertions and
// changes state, and somebody has to notice and answer. That somebody used to
// be nobody, which is why no table could form.
func (r *Runtime) advance(ctx context.Context, t *table, from where) {
	learned := len(t.form.Joins()) > from.joins
	moved := t.form.State() != from.state

	switch t.form.State() {
	case membership.Joining:
		if learned {
			// What heals a channel that loses messages: a peer that
			// missed somebody's join learns it here, with the
			// signature that lets it check it.
			r.say(ctx, t, "what we hold", r.publishRoster)
		}

	case membership.Formed:
		if learned || moved {
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
		if t.form.Agreed() || t.form.WindowClosed() {
			c, err := t.form.Bind()
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
			r.advance(ctx, t, where{state: membership.Formed, joins: len(t.form.Joins())})
		}

	case membership.Committed:
		if learned || moved {
			r.say(ctx, t, "what we hold", r.publishRoster)
		}

	case membership.Settled:
		if err := r.seatIfReady(ctx, t.match); err != nil {
			r.log.Debugf("table %s: not seated yet: %v", t.match, err)
		}

	case membership.Aborted:
		if moved {
			r.log.Warnf("table %s did not form: %s", t.match, t.form.Reason())
			r.keep(t)
		}
	}
}

// say runs one of the publishing steps and logs rather than fails. Nothing
// here is worth unwinding a formation over; the block repeat covers a message
// that did not get out.
func (r *Runtime) say(ctx context.Context, t *table, what string, f func(context.Context, string) error) {
	if err := f(ctx, t.match); err != nil {
		r.log.Warnf("table %s: saying %s: %v", t.match, what, err)
	}
}

// Resync asks every live table for what this peer is missing.
//
// Called when the bridge says it dropped frames. Formation messages are
// published when something happens, so a peer whose stream was down while
// somebody committed is short a signature its table needs to settle, and
// nothing would send it again. Saying what we hold is not enough on its own -
// the gap may be in what we never heard - so this asks, naming what it has, and
// the table answers with the difference.
func (r *Runtime) Resync(ctx context.Context) {
	r.mu.Lock()
	tables := make([]*table, 0, len(r.tables))
	for _, t := range r.tables {
		tables = append(tables, t)
	}
	r.mu.Unlock()

	for _, t := range tables {
		if t.form.State() == membership.Aborted {
			continue
		}
		r.say(ctx, t, "what we are missing", r.publishResync)
	}
}

// publishResync asks the table for what this peer does not have, by naming
// what it does.
func (r *Runtime) publishResync(ctx context.Context, match string) error {
	t, err := r.tableOf(match)
	if err != nil {
		return err
	}
	return r.send(ctx, t, KindResync, resyncAsk(t))
}

// resyncAsk names everything this peer holds.
//
// Naming it is the whole economy of the exchange: an ask that said only "catch
// me up" would be answered with the entire membership, on every reconnect, by
// every peer.
func resyncAsk(t *table) schema.Resync {
	ask := schema.Resync{}
	for _, j := range t.form.Joins() {
		ask.Joins = append(ask.Joins, hex.EncodeToString(j.Key))
	}
	for _, c := range t.form.Commits() {
		ask.Commits = append(ask.Commits, hex.EncodeToString(c.Signer))
	}
	return ask
}

// answerResync sends back what the asker did not name.
//
// Only the difference, and nothing at all when there is none: an answer that
// repeated the whole table every time somebody reconnected would be the
// largest message this protocol sends, and the one most often sent.
func (r *Runtime) answerResync(ctx context.Context, match string, ask schema.Resync) error {
	t, err := r.tableOf(match)
	if err != nil {
		return err
	}
	reply := resyncDiff(t, ask)
	if len(reply.Joins) == 0 && len(reply.Commits) == 0 {
		return nil
	}
	return r.send(ctx, t, KindResyncReply, reply)
}

// resyncDiff is what the asker did not name.
func resyncDiff(t *table, ask schema.Resync) schema.ResyncReply {
	named := func(keys []string) map[string]struct{} {
		out := make(map[string]struct{}, len(keys))
		for _, k := range keys {
			out[strings.ToLower(strings.TrimSpace(k))] = struct{}{}
		}
		return out
	}
	hasJoin, hasCommit := named(ask.Joins), named(ask.Commits)

	var reply schema.ResyncReply
	for _, j := range t.form.Joins() {
		if _, ok := hasJoin[hex.EncodeToString(j.Key)]; !ok {
			reply.Joins = append(reply.Joins, schema.JoinFrom(j))
		}
	}
	for _, c := range t.form.Commits() {
		if _, ok := hasCommit[hex.EncodeToString(c.Signer)]; !ok {
			reply.Commits = append(reply.Commits, schema.CommitFrom(c))
		}
	}
	return reply
}

// adoptResync folds in what somebody sent to catch this peer up.
//
// This trusts the sender for nothing. Everything here is signed by the member
// it concerns and checked on the way in, so a peer answering with keys nobody
// joined with, or a commit it forged, has them refused exactly as it would had
// it published them directly.
func (r *Runtime) adoptResync(ctx context.Context, match string, body schema.ResyncReply) error {
	t, err := r.tableOf(match)
	if err != nil {
		return err
	}
	from := at(t)
	for i, wj := range body.Joins {
		j, err := wj.Into()
		if err != nil {
			return fmt.Errorf("resync join %d: %w", i, err)
		}
		if err := t.form.AddJoin(j); err != nil {
			return fmt.Errorf("resync join %d: %w", i, err)
		}
	}
	for i, wc := range body.Commits {
		c, err := wc.Into()
		if err != nil {
			return fmt.Errorf("resync commit %d: %w", i, err)
		}
		if err := t.form.AddCommit(c); err != nil {
			return fmt.Errorf("resync commit %d: %w", i, err)
		}
	}
	r.keep(t)
	r.advance(ctx, t, from)
	return nil
}
