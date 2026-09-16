package membership

import (
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
)

// State is where one peer has got to in forming a table.
type State uint8

const (
	// Joining is still collecting joins. No membership exists yet.
	Joining State = iota

	// Formed holds exactly the seats the table needs, so a membership can
	// be computed - but this peer is not bound to it yet, and a further
	// join would still abort.
	Formed

	// Committed means this peer has bound itself and will never sign
	// another roster for this session. Later joins are ignored from here,
	// which is what makes the binding worth anything.
	Committed

	// Settled means every member bound itself to the same membership.
	Settled

	// Aborted is terminal. It must be persisted: a commit arriving after
	// the others gave up would otherwise resurrect this peer into a roster
	// nobody else is bound to.
	Aborted
)

func (s State) String() string {
	switch s {
	case Joining:
		return "joining"
	case Formed:
		return "formed"
	case Committed:
		return "committed"
	case Settled:
		return "settled"
	case Aborted:
		return "aborted"
	}
	return "unknown"
}

// Formation is one peer's view of a table forming, with no referee.
//
// Every peer runs this and reaches the membership itself; nobody proposes one
// and nobody closes the table. The rule is exact fill: a membership exists only
// when the joins held are exactly the seats the table has.
//
// That is deliberately not "the lowest N keys of whatever has arrived", which
// is the obvious rule and does not work. Healing a lossy channel only ever
// grows the set of joins a peer holds, and the lowest N of a set is not
// monotone as the set grows - one low key arriving late ejects the previous
// highest member. A peer could therefore settle around a member another peer
// had already dropped. Under exact fill a peer holding a strict subset computes
// no membership at all and stays silent, so being under-informed is
// self-evident rather than indistinguishable from being well-informed.
type Formation struct {
	mu    sync.Mutex
	terms Terms
	creds Credentials
	self  []byte // our own compressed session key

	joins   map[string]*Join    // key hex -> join
	commits map[string]*Commit  // signer hex -> the one commit they made
	asserts map[string][32]byte // signer hex -> the membership they last claimed
	closed  bool                // the admission window has shut

	state     State
	reason    string
	canonical [][]byte
	roster    [32]byte
	conflict  *ConflictingCommits
	// beacon is the block hash the seating was drawn from. Until it is
	// known there is no seating, only a membership - which is the whole
	// point: seats must not be decided by anything a player chose.
	beacon []byte
}

// NewFormation starts forming the table these terms describe, joining it with
// the given session key.
func NewFormation(t Terms, c Credentials) (*Formation, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	ours, err := SignJoin(t, c)
	if err != nil {
		return nil, err
	}
	f := &Formation{
		terms:   t,
		creds:   c,
		self:    ours.Key,
		joins:   make(map[string]*Join),
		commits: make(map[string]*Commit),
		asserts: make(map[string][32]byte),
	}
	if err := f.lockedAddJoin(ours); err != nil {
		return nil, err
	}
	return f, nil
}

// Terms reports what this table was formed under.
func (f *Formation) lockedTerms() Terms { return f.terms }

// State reports where forming has got to.
func (f *Formation) lockedState() State { return f.state }

// Reason explains an abort, for a person rather than for code.
func (f *Formation) lockedReason() string { return f.reason }

// Ours is this peer's own join, the one to publish.
func (f *Formation) lockedOurs() *Join { return f.joins[keyID(f.self)] }

// Joins reports every verified join held, in canonical key order. This is what
// a roster assertion carries, so a peer that missed one can learn it from
// anyone who did and check it rather than take their word.
func (f *Formation) lockedJoins() []*Join {
	out := make([]*Join, 0, len(f.joins))
	for _, k := range sortKeys(f.keys()) {
		out = append(out, f.joins[keyID(k)])
	}
	return out
}

// Commits reports every commit held, in canonical signer order.
//
// A peer that restarts has to come back holding these. Settling needs a commit
// from every member, and a commit that only ever lived in memory leaves a table
// that had already agreed unable to say so - it waits for a message that was
// sent once and will not be sent again.
func (f *Formation) lockedCommits() []*Commit {
	keys := make([][]byte, 0, len(f.commits))
	for _, c := range f.commits {
		keys = append(keys, c.Signer)
	}
	out := make([]*Commit, 0, len(f.commits))
	for _, k := range sortKeys(keys) {
		out = append(out, f.commits[keyID(k)])
	}
	return out
}

// CloseWindow shuts admission, which the caller does when it sees the chain
// past the deadline the terms name.
//
// This is what makes "no more joins are coming" a fact rather than a hope. A
// table still short of its seats at that point cannot form, and one that has
// exactly its seats can bind knowing nothing will displace it.
//
// The height is not read here. Formation stays a pure function of what it has
// been told, so the one thing that has to consult a clock or a chain is the
// caller - and it can be tested by saying so directly.
func (f *Formation) lockedCloseWindow() {
	if f.closed || f.state == Aborted || f.state == Settled {
		return
	}
	f.closed = true
	if f.state == Joining {
		// Short of a full table when admission shut. Nothing else can
		// arrive, so there is nothing left to wait for.
		f.abort(fmt.Sprintf("only %d of %d seats were taken before the deadline",
			len(f.joins), f.terms.Seats))
	}
}

// WindowClosed reports whether admission has shut.
func (f *Formation) lockedWindowClosed() bool { return f.closed }

// Agreed reports whether every member of this peer's membership has said it
// holds the same one.
//
// It is what lets a full table form without waiting out its deadline. Everybody
// having asserted the same membership is not proof that no straggler exists -
// only the deadline is that - but it does mean every member has seen exactly
// this set, which is enough to bind on when the alternative is a lobby nobody
// is watching for ten minutes.
func (f *Formation) lockedAgreed() bool {
	if f.canonical == nil {
		return false
	}
	for _, k := range f.canonical {
		if got, ok := f.asserts[keyID(k)]; !ok || got != f.roster {
			return false
		}
	}
	return true
}

// AddAssertion records what another peer says it holds, and adopts the joins it
// showed to back the claim.
//
// The joins are what make the claim checkable, and they are checked before any
// is kept: rejection has to be wholesale, or a member could get a key nobody
// joined with admitted by burying it among real ones.
func (f *Formation) lockedAddAssertion(a *Assertion, joins []*Join) error {
	if f.state == Aborted || f.state == Settled {
		return nil
	}
	if a == nil {
		return fmt.Errorf("no assertion")
	}
	if err := a.Verify(f.terms); err != nil {
		return fmt.Errorf("assertion: %w", err)
	}
	for i, j := range joins {
		if j == nil {
			return fmt.Errorf("assertion join %d: missing", i)
		}
		if err := j.Verify(f.terms); err != nil {
			return fmt.Errorf("assertion join %d: %w", i, err)
		}
	}

	f.asserts[keyID(a.Signer)] = a.Roster
	for _, j := range joins {
		if err := f.lockedAddJoin(j); err != nil {
			return err
		}
	}
	f.recompute()
	return nil
}

// AddJoin admits one join, whether it arrived from its author or was relayed
// inside somebody's roster assertion.
//
// A join that fails to verify is refused and changes nothing. That matters most
// for relayed ones: rejection has to be wholesale, or a member could inject a
// key nobody joined with by burying it among real joins.
func (f *Formation) lockedAddJoin(j *Join) error {
	if f.state == Aborted || f.state == Settled {
		return nil
	}
	if j == nil {
		return fmt.Errorf("no join")
	}
	if err := j.Verify(f.terms); err != nil {
		return fmt.Errorf("join: %w", err)
	}
	id := keyID(j.Key)
	if _, have := f.joins[id]; have {
		// Re-delivery is ordinary: joins are re-broadcast because the
		// wire layer never retransmits. Accumulating a set makes that
		// idempotent for free.
		return nil
	}

	if f.state == Committed {
		// Bound already. A late arrival cannot change what this peer
		// signed, and must not: that is the whole point of committing.
		return nil
	}
	if f.closed {
		// Admission shut. Accepting now would be accepting a join the
		// peers who saw the deadline pass have already refused.
		return nil
	}

	f.joins[id] = j
	f.recompute()
	return nil
}

// AddCommit admits one peer's binding.
//
// Commits are accepted before this peer has a membership of its own, because
// the joins they were computed from may still be in flight. What cannot be
// accepted is a second, different commit from a key that already made one -
// that is a contradiction, and it ends the table with the proof retained.
func (f *Formation) lockedAddCommit(c *Commit) error {
	if f.state == Aborted {
		return nil
	}
	if c == nil {
		return fmt.Errorf("no commit")
	}
	if err := c.Verify(f.terms); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	id := keyID(c.Signer)
	if prev, have := f.commits[id]; have {
		if prev.Roster == c.Roster {
			return nil
		}
		// Keep the proof either way, but do not unwind a table that
		// already settled. Settled is terminal, and it is still sound:
		// this member bound itself to ours as well, so satisfying our
		// escrows still needs every signature it always did - and to
		// make the other table real they would have to fund it too.
		f.conflict = &ConflictingCommits{A: prev, B: c}
		if f.state != Settled {
			f.abort(fmt.Sprintf("member %s… bound itself to two different rosters", id[:8]))
		}
		return nil
	}
	f.commits[id] = c
	f.recompute()
	return nil
}

// Bind commits this peer to the membership it holds, irrevocably.
//
// It is a separate step from reaching Formed, and the caller's to take, because
// when to take it is a policy question this type cannot answer: binding early
// risks binding to a membership a late join would have aborted, and binding
// late risks the table never forming at all. What this type guarantees is that
// once bound, nothing moves it.
func (f *Formation) lockedBind() (*Commit, error) {
	switch f.state {
	case Committed, Settled:
		return f.commits[keyID(f.self)], nil
	case Formed:
	default:
		return nil, fmt.Errorf("cannot bind while %s", f.state)
	}

	c, err := SignCommit(f.terms, f.roster, f.creds.Session)
	if err != nil {
		return nil, err
	}
	f.commits[keyID(c.Signer)] = c
	f.state = Committed
	f.recompute()
	return c, nil
}

// Assertion is this peer's own claim about what it holds, to publish.
//
// Making one records it, because this peer's own view counts toward agreement
// exactly as anyone else's does - and a caller that had to remember to file its
// own would eventually not.
func (f *Formation) lockedAssertion() (*Assertion, error) {
	if f.canonical == nil {
		return nil, fmt.Errorf("no membership to assert yet")
	}
	a, err := SignAssertion(f.terms, f.roster, f.creds.Session)
	if err != nil {
		return nil, err
	}
	f.asserts[keyID(a.Signer)] = a.Roster
	return a, nil
}

// Members reports the membership in canonical key order, which is the order
// every escrow script names them in and the order signatures are supplied in.
// It is available from Formed onwards.
func (f *Formation) lockedMembers() ([][]byte, bool) {
	if f.canonical == nil {
		return nil, false
	}
	out := make([][]byte, len(f.canonical))
	copy(out, f.canonical)
	return out, true
}

// SetBeacon supplies the block the seating is drawn from.
//
// The caller reads it, because Formation touches no chain. What it must not do
// is read it early: the block has to be one that did not exist when the members
// were choosing their keys, or the draw is something a player can grind.
func (f *Formation) lockedSetBeacon(hash []byte) error {
	if len(hash) == 0 {
		return fmt.Errorf("no beacon")
	}
	if f.beacon != nil {
		// Seating is drawn once. A second beacon would reseat a table
		// that may already have been dealt.
		return nil
	}
	f.beacon = append([]byte(nil), hash...)
	return nil
}

// BeaconHeight is the block this table draws its seating from.
func (f *Formation) lockedBeaconHeight() uint32 { return BeaconHeight(f.terms) }

// Seated reports whether the seating has been drawn.
func (f *Formation) lockedSeated() bool { return f.beacon != nil && f.canonical != nil }

// Beacon is the block hash the seating was drawn from, or nil before the draw.
//
// It is worth keeping rather than re-reading: the height is derivable from the
// terms, so a restart could fetch the block again, but only while that block is
// still the one at that height. Storing what was actually drawn from means a
// reorganisation cannot quietly reseat a table that has already been dealt.
func (f *Formation) lockedBeacon() []byte {
	if f.beacon == nil {
		return nil
	}
	return append([]byte(nil), f.beacon...)
}

// Seats reports which key holds which seat.
//
// It is empty until the beacon arrives, and that is the point rather than a
// limitation. Seat order decides the button, so seating by canonical key order
// would hand the first hand's button to whoever generated the lowest key - and
// escrow.CanonicalMembers says of its own ordering that it holds "regardless of
// seat assignment". The two were only ever the same thing for want of anything
// better to be the other.
func (f *Formation) lockedSeats() (map[uint32][]byte, bool) {
	if !f.lockedSeated() {
		return nil, false
	}
	seats, err := SeatOrder(f.roster, f.beacon, f.canonical)
	if err != nil {
		return nil, false
	}
	return seats, true
}

// LogSeats maps each seat to the key that seat signs the action log with.
//
// This is what pkg/gamelog verifies entries against, and it is deliberately not
// the same map as Seats. Those are session keys: they hold the stakes, they are
// named in the escrow scripts, and they must never be the keys that sign the
// log - a log signature's nonce comes from its position, so equivocating
// publishes the signing key, and publishing the key that holds your stake would
// hand the escrow to whoever was watching rather than the bond to whoever you
// cheated.
//
// Derived from the joins rather than announced separately, so the two cannot
// come apart. A join is signed by the session key over a digest covering the
// log key, which makes the join itself the binding: nothing here is taken on
// anybody's word, and there is no second message to go missing on a channel
// that loses messages.
func (f *Formation) lockedLogSeats() (map[uint32][]byte, bool) {
	seats, ok := f.lockedSeats()
	if !ok {
		return nil, false
	}
	out := make(map[uint32][]byte, len(seats))
	for seat, session := range seats {
		j := f.joins[keyID(session)]
		if j == nil || len(j.LogKey) != escrow.PubKeyLen {
			return nil, false
		}
		out[seat] = append([]byte(nil), j.LogKey...)
	}
	return out, true
}

// TableBonds derives every seat's forfeitable bond from this table's seating.
//
// Empty until the seating is drawn, for the same reason the deposits are: the
// bonds name the membership, and a membership that could still change is one
// nobody should be paying into.

// OurSeat reports which seat this peer holds.
func (f *Formation) lockedOurSeat() (uint32, bool) {
	seats, ok := f.lockedSeats()
	if !ok {
		return 0, false
	}
	self := keyID(f.self)
	for seat, key := range seats {
		if keyID(key) == self {
			return seat, true
		}
	}
	return 0, false
}

// Deposits reports where every seat's stake has to be paid.
//
// It needs the seating and not merely the membership, because a deposit belongs
// to a seat and until the beacon is drawn there are no seats to attribute one
// to. Everything else is already agreed: the keys, and the timelock each
// member's own refund branch carries.
//
// Every peer derives all of them, including the ones it will never pay. Under a
// referee a client was handed an address and had to rebuild the script from the
// roster it was sent, to check the two matched - because a referee that swapped
// in a key of its own would otherwise collect the table. Here nobody is handed
// anything, which removes that check by removing the party that could be wrong.
func (f *Formation) lockedDeposits(params stdaddr.AddressParams) ([]Deposit, error) {
	seats, ok := f.lockedFundingSeats()
	if !ok {
		return nil, fmt.Errorf("this table has no seating yet")
	}
	members := make([]Member, 0, len(seats))
	for seat, key := range seats {
		members = append(members, Member{
			Seat:       seat,
			CompPubkey: key,
			CSVBlocks:  f.terms.CSVBlocks,
		})
	}
	_, deposits, err := Close(members, params)
	if err != nil {
		return nil, err
	}
	return deposits, nil
}

// RosterHash is what a commit binds to. Available from Formed onwards.
func (f *Formation) lockedRosterHash() ([32]byte, bool) {
	if f.canonical == nil {
		return [32]byte{}, false
	}
	return f.roster, true
}

// MatchID identifies the match everything else hangs off: the signed action
// log, and the frames that carry it.
//
// It is the roster hash rather than the session id because the session id is
// only a routing key - it says which conversation frames belong to, and two
// groups that somehow formed different tables under one invitation would
// collide on it. The roster hash cannot collide, because it is what they
// disagreed about.
func (f *Formation) lockedMatchID() (string, bool) {
	if f.canonical == nil {
		return "", false
	}
	return hex.EncodeToString(f.roster[:]), true
}

// Roster renders the settled membership in the shape the escrow layer wants.
// It needs the seating as well as the agreement, so it is empty until the
// beacon has been drawn.
func (f *Formation) lockedRoster() (*Roster, bool) {
	seats, ok := f.lockedSeats()
	if !ok || f.state != Settled {
		return nil, false
	}
	members, _ := f.lockedMembers()
	return &Roster{
		Size:    int(f.terms.Seats),
		Seats:   seats,
		Escrows: make(map[uint32]string),
		Members: members,
	}, true
}

// Conflict returns the proof that ended the table, if that is what ended it.
func (f *Formation) lockedConflict() *ConflictingCommits { return f.conflict }

// recompute advances the state machine after anything was admitted.
func (f *Formation) recompute() {
	switch f.state {
	case Aborted, Settled:
		return
	}

	// Membership first, since everything after it needs one.
	switch {
	case f.state == Committed:
		// Bound: the membership is fixed and joins no longer move it.
	case len(f.joins) > int(f.terms.Seats):
		f.abort(fmt.Sprintf("%d players answered a %d seat table", len(f.joins), f.terms.Seats))
		return
	case len(f.joins) == int(f.terms.Seats):
		canonical, err := escrow.CanonicalMembers(f.keys())
		if err != nil {
			f.abort(fmt.Sprintf("membership is not usable: %v", err))
			return
		}
		roster, err := RosterHash(f.terms, canonical)
		if err != nil {
			f.abort(fmt.Sprintf("membership is not usable: %v", err))
			return
		}
		f.canonical, f.roster, f.state = canonical, roster, Formed
	default:
		// Still short of a full table. Nothing to compute, and nothing
		// to say: a peer holding a subset must stay silent rather than
		// assert a membership it has no reason to believe is whole.
		f.canonical, f.state = nil, Joining
		return
	}

	if f.state != Committed {
		return
	}

	// Settled needs every member of our own membership bound to our own
	// roster. Anyone else's commit is somebody else's table.
	for _, k := range f.canonical {
		c, ok := f.commits[keyID(k)]
		if !ok || c.Roster != f.roster {
			return
		}
	}
	f.state = Settled
}

// Abandon gives up on a table that bound itself and got nothing back.
//
// It keeps the membership, which is what makes it different from every other
// abort here. Aborting during formation clears the canonical set because there
// was never a table; abandoning one that settled must not, because a member who
// did pay needs their own deposit script to take the refund branch and that
// script is derived from the membership. Forgetting it would be forgetting
// where their money is.
//
// Committed is kept for the same reason one step earlier: the commit is this
// key's irrevocable word on a roster, and a table stuck there - bound, with
// somebody else's commitment never arriving - must end without unsaying it.
// No money can have moved at Committed: funding needs a seating, and seating
// needs the table settled. Anything before Committed has its own way of
// ending, and anything after Settled has money moving under rules this does
// not know.
func (f *Formation) lockedAbandon(reason string) {
	if f.state != Settled && f.state != Committed {
		return
	}
	f.state = Aborted
	f.reason = reason
}

func (f *Formation) abort(reason string) {
	f.state = Aborted
	f.reason = reason
	f.canonical = nil
}

func (f *Formation) keys() [][]byte {
	out := make([][]byte, 0, len(f.joins))
	for _, j := range f.joins {
		out = append(out, j.Key)
	}
	return out
}

func (f *Formation) Terms() Terms       { f.mu.Lock(); defer f.mu.Unlock(); return f.lockedTerms() }
func (f *Formation) State() State       { f.mu.Lock(); defer f.mu.Unlock(); return f.lockedState() }
func (f *Formation) Reason() string     { f.mu.Lock(); defer f.mu.Unlock(); return f.lockedReason() }
func (f *Formation) Ours() *Join        { f.mu.Lock(); defer f.mu.Unlock(); return f.lockedOurs() }
func (f *Formation) Joins() []*Join     { f.mu.Lock(); defer f.mu.Unlock(); return f.lockedJoins() }
func (f *Formation) Commits() []*Commit { f.mu.Lock(); defer f.mu.Unlock(); return f.lockedCommits() }
func (f *Formation) CloseWindow()       { f.mu.Lock(); defer f.mu.Unlock(); f.lockedCloseWindow() }
func (f *Formation) WindowClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedWindowClosed()
}
func (f *Formation) Agreed() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.lockedAgreed() }
func (f *Formation) AddAssertion(a *Assertion, joins []*Join) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedAddAssertion(a, joins)
}
func (f *Formation) AddJoin(j *Join) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedAddJoin(j)
}
func (f *Formation) AddCommit(c *Commit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedAddCommit(c)
}
func (f *Formation) Bind() (*Commit, error) { f.mu.Lock(); defer f.mu.Unlock(); return f.lockedBind() }
func (f *Formation) Assertion() (*Assertion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedAssertion()
}
func (f *Formation) Members() ([][]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedMembers()
}
func (f *Formation) SetBeacon(hash []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedSetBeacon(hash)
}
func (f *Formation) BeaconHeight() uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedBeaconHeight()
}
func (f *Formation) Seated() bool   { f.mu.Lock(); defer f.mu.Unlock(); return f.lockedSeated() }
func (f *Formation) Beacon() []byte { f.mu.Lock(); defer f.mu.Unlock(); return f.lockedBeacon() }
func (f *Formation) Seats() (map[uint32][]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedSeats()
}
func (f *Formation) LogSeats() (map[uint32][]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedLogSeats()
}

func (f *Formation) OurSeat() (uint32, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedOurSeat()
}
func (f *Formation) Deposits(params stdaddr.AddressParams) ([]Deposit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedDeposits(params)
}
func (f *Formation) RosterHash() ([32]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedRosterHash()
}
func (f *Formation) MatchID() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedMatchID()
}
func (f *Formation) Roster() (*Roster, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedRoster()
}
func (f *Formation) Conflict() *ConflictingCommits {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedConflict()
}
func (f *Formation) Abandon(reason string) { f.mu.Lock(); defer f.mu.Unlock(); f.lockedAbandon(reason) }

// FundingSeats names financial authority, separate from the game/log identities.
// Every member must bind an independent bridge-controlled spending key.
func (f *Formation) FundingSeats() (map[uint32][]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedFundingSeats()
}
func (f *Formation) lockedFundingSeats() (map[uint32][]byte, bool) {
	seats, ok := f.lockedSeats()
	if !ok {
		return nil, false
	}
	out := make(map[uint32][]byte, len(seats))
	for seat, key := range seats {
		j := f.joins[keyID(key)]
		if j == nil {
			return nil, false
		}
		terms, err := escrow.ParseBond(j.Bond.Script)
		if err != nil {
			return nil, false
		}
		if len(terms.Recovery) != 0 {
			out[seat] = append([]byte(nil), terms.Recovery...)
		} else {
			return nil, false
		}
	}
	return out, true
}
