package runtime

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

// Tables have to survive a restart, and this is where they are kept.
//
// A daemon holding real coin gets restarted - for an upgrade, by a reboot, by
// a crash - and a table it forgot is a table whose stake nobody will settle and
// whose bond nobody will release. Both existing games worked this out and both
// wrote it themselves, which is the whole reason this file is here.
//
// What is kept is the membership and the money: the terms, the joins and
// commits that decided the seating, the beacon, and where each output landed.
// What is not kept is anything derivable - every seat key, including this
// seat's punishment key, comes back out of the game's seed and the roster, so
// no key is ever written down.

// TableRecord is one table, in a form that can be written down and read back.
//
// Terms is the whole of membership.Terms rather than the wire's rendering of
// it, because the wire carries terms only so peers can say they disagree, and
// it leaves out the bond fields. A table resumed through that rendering would
// come back with no bond terms, hash to something else, and refuse every commit
// it had already collected.
type TableRecord struct {
	BridgePayout   string `json:"bridgePayout"`
	PayoutID       string `json:"payoutID,omitempty"`
	BridgeKey      string `json:"bridgeKey,omitempty"`
	Version        uint32 `json:"version,omitempty"`
	RecoveryOnly   bool   `json:"recoveryOnly,omitempty"`
	RecoveryReason string `json:"recoveryReason,omitempty"`
	Match          string `json:"match"`
	GCID           string `json:"gcid"`

	Terms   membership.Terms `json:"terms"`
	Joins   []schema.Join    `json:"joins,omitempty"`
	Commits []schema.Commit  `json:"commits,omitempty"`
	Beacon  string           `json:"beacon,omitempty"`

	// Bound and Roster are this seat's own commitment. Kept together and
	// checked against each other on the way back in: see Runtime.resume for
	// why a table that would bind to a different roster is refused.
	Bound  bool   `json:"bound,omitempty"`
	Roster string `json:"roster,omitempty"`

	Aborted bool   `json:"aborted,omitempty"`
	Reason  string `json:"reason,omitempty"`

	// Where the money went. A seat's stake, its table bond and its
	// forfeitable bond each land in their own output.
	// SeatBond is what this seat's join binds to, for a game that posts one
	// per table. Empty where the bond is the identity's.
	SeatBond Funded            `json:"seatBond,omitempty"`
	Funded   map[uint32]Funded `json:"funded,omitempty"`
	Payouts  map[uint32]string `json:"payouts,omitempty"`

	// Game is whatever the game keeps alongside this table, untouched. Only
	// a game that implements Persisting has one.
	Game json.RawMessage `json:"game,omitempty"`
}

// Funded is one output that has been paid and found.
type Funded struct {
	Outpoint string `json:"outpoint"`
	Atoms    int64  `json:"atoms"`
}

// TableStore is where a runtime keeps its tables between runs.
//
// One record at a time rather than the whole set, because tables are
// independent: a table that cannot be written is one table lost, and taking the
// rest down with it would turn a bad file into a forgotten stake.
type TableStore interface {
	// LoadTables reads back every table that was written down.
	LoadTables() ([]TableRecord, error)
	// SaveTable writes one table, replacing any earlier version of it.
	SaveTable(TableRecord) error
	// DropTable forgets one, for a table that is over.
	DropTable(match string) error
}

// Persisting is an optional hook. A game that implements it keeps its own state
// alongside the runtime's, in the same record and written at the same moment,
// so the two cannot come back out of step with each other.
//
// Optional because most of what a game needs is already in the record: who is
// seated, what they staked, where it landed. This is for what the runtime has
// no vocabulary for - whose turn it is, which hand is disputed.
type Persisting interface {
	// SaveTable is the game's own state for a table, as it will be stored.
	// Returning nil keeps nothing, which is not an error.
	SaveTable(match string) (json.RawMessage, error)
	// LoadTable hands it back on the way up. Called before the table is
	// live, so the game can be ready before the first message arrives.
	LoadTable(match string, blob json.RawMessage) error
}

// snapshot renders a table as it stands. Caller must not hold r.mu.
func (r *Runtime) snapshot(t *table) TableRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked(t)
}

// snapshotLocked renders a table as it stands. Caller holds r.mu.
func (r *Runtime) snapshotLocked(t *table) TableRecord {
	rec := TableRecord{Version: 2, BridgePayout: t.bridgePayout, PayoutID: t.payoutID, BridgeKey: t.bridgeKey, Match: t.match, GCID: t.gcID, Terms: t.terms, RecoveryOnly: t.recoveryOnly, RecoveryReason: t.recoveryReason}
	if t.formation() == nil {
		// Accepted but not yet joined: the terms and whatever its seat
		// bond has cost so far, which is the whole of what it knows.
		rec.SeatBond = Funded{Outpoint: t.seatBond.outpoint, Atoms: t.seatBond.atoms}
		return rec
	}
	rec.Terms = t.formation().Terms()
	for _, j := range t.formation().Joins() {
		rec.Joins = append(rec.Joins, schema.JoinFrom(j))
	}
	for _, c := range t.formation().Commits() {
		rec.Commits = append(rec.Commits, schema.CommitFrom(c))
	}
	if b := t.formation().Beacon(); len(b) > 0 {
		rec.Beacon = hex.EncodeToString(b)
	}
	if h, ok := t.formation().RosterHash(); ok && bound(t.formation().State()) {
		rec.Bound, rec.Roster = true, hex.EncodeToString(h[:])
	}
	if t.formation().State() == membership.Aborted {
		rec.Aborted, rec.Reason = true, t.formation().Reason()
	}

	rec.SeatBond = Funded{Outpoint: t.seatBond.outpoint, Atoms: t.seatBond.atoms}
	rec.Funded = fundedOf(t.funded)
	for seat, pay := range t.payouts {
		if rec.Payouts == nil {
			rec.Payouts = map[uint32]string{}
		}
		rec.Payouts[seat] = hex.EncodeToString(pay)
	}

	return rec
}

// bound reports whether this seat has committed to a roster. Not a comparison
// against the enum's order: Aborted sorts after both of these and is the one
// state where no commitment stands.
func bound(s membership.State) bool {
	return s == membership.Committed || s == membership.Settled
}

func fundedOf(in map[uint32]staked) map[uint32]Funded {
	if len(in) == 0 {
		return nil
	}
	out := make(map[uint32]Funded, len(in))
	for seat, s := range in {
		out[seat] = Funded{Outpoint: s.outpoint, Atoms: s.atoms}
	}
	return out
}

func stakedOf(in map[uint32]Funded) map[uint32]staked {
	out := make(map[uint32]staked, len(in))
	for seat, f := range in {
		out[seat] = staked{outpoint: f.Outpoint, atoms: f.Atoms}
	}
	return out
}

// keep writes a table down, including whatever the game keeps with it.
//
// A failed write latches a storage fault. Subsequent signing, funding and
// broadcast are blocked until the runtime is reopened from durable state.
func (r *Runtime) keep(t *table) error {
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	if err := r.healthy(); err != nil {
		return err
	}
	if r.store == nil {
		return nil
	}
	return r.write(t, r.snapshot(t))
}
func (r *Runtime) write(t *table, rec TableRecord) error {
	if err := r.healthy(); err != nil {
		return err
	}
	if p, ok := r.rules.(Persisting); ok {
		blob, err := p.SaveTable(t.match)
		if err != nil {
			return r.storageFault(err)
		}
		rec.Game = blob
	}
	if err := r.store.SaveTable(rec); err != nil {
		return r.storageFault(err)
	}
	return nil
}

// Resume takes back up every table that was written down.
//
// Called before Run. A table that will not resume is reported and skipped
// rather than taken down with the rest: one unreadable record must not cost
// every other table its settlement.
func (r *Runtime) Resume() error {
	r.resumeReport = ResumeReport{Failed: map[string]string{}}
	if r.store == nil {
		return nil
	}
	recs, loadErr := r.store.LoadTables()
	if loadErr != nil {
		r.resumeReport.Failed["storage"] = loadErr.Error()
	}
	// In a fixed order, so a run that resumes ten tables logs the same way
	// twice and a failure can be found again.
	sort.Slice(recs, func(i, j int) bool { return recs[i].Match < recs[j].Match })

	var failed int
	for _, rec := range recs {
		if err := r.resume(rec); err != nil {
			failed++
			r.resumeReport.Failed[rec.Match] = err.Error()
			r.log.Errorf("table %s did not come back: %v", rec.Match, err)
		} else if rec.RecoveryOnly || rec.Aborted {
			r.resumeReport.RecoveryOnly = append(r.resumeReport.RecoveryOnly, rec.Match)
		} else {
			r.resumeReport.Restored = append(r.resumeReport.Restored, rec.Match)
		}
	}
	if failed > 0 {
		r.log.Warnf("%d of %d tables did not come back", failed, len(recs))
	}
	return loadErr
}

// resume rebuilds one table from its record.
func (r *Runtime) resume(rec TableRecord) error {
	if rec.Version != 2 {
		return fmt.Errorf("unsupported table record version %d", rec.Version)
	}
	if err := rec.Terms.Validate(); err != nil {
		return fmt.Errorf("invalid stored terms: %w", err)
	}
	if rec.Terms.SID != rec.Match || !gcID.MatchString(rec.GCID) {
		return fmt.Errorf("invalid stored table identity")
	}
	if rec.Match == "" {
		return fmt.Errorf("a record with no table")
	}
	heldPayment := false
	for _, payment := range r.book.All() {
		if payment.Match == rec.Match && payment.State != spend.Denied && payment.State != spend.Expired {
			heldPayment = true
		}
	}
	if rec.Aborted && !heldPayment && rec.SeatBond.Outpoint == "" && len(rec.Funded) == 0 {
		// Terminal, and it stays terminal. Rebuilt as a tombstone
		// rather than a table so a commit that arrives after everyone
		// else gave up cannot put this process back into a membership
		// nobody is bound to.
		r.mu.Lock()
		r.ended[rec.Match] = rec.Reason
		r.mu.Unlock()
		return nil
	}
	// The table first, because a per-table seat bond is the table's and the
	// credentials cannot be built without knowing where it is.
	t := &table{
		match: rec.Match, gcID: rec.GCID, terms: rec.Terms, bridgeKey: rec.BridgeKey, payoutID: rec.PayoutID, bridgePayout: rec.BridgePayout, recoveryOnly: rec.RecoveryOnly || rec.Aborted, recoveryReason: rec.RecoveryReason,
		seatBond: staked{outpoint: rec.SeatBond.Outpoint, atoms: rec.SeatBond.Atoms},
		funded:   stakedOf(rec.Funded),
		payouts:  map[uint32][]byte{},
	}
	if len(rec.Joins) == 0 && !rec.Bound && rec.Beacon == "" {
		if p, ok := r.rules.(Persisting); ok && len(rec.Game) > 0 {
			if err := p.LoadTable(rec.Match, rec.Game); err != nil {
				return err
			}
		}
		r.mu.Lock()
		r.tables[rec.Match] = t
		r.mu.Unlock()
		return nil
	}
	creds, err := r.seatCredentials(t, rec.Terms)
	if err != nil {
		return err
	}
	form, err := membership.NewFormation(rec.Terms, creds)
	if err != nil {
		return fmt.Errorf("rebuild the formation: %w", err)
	}
	for i, wj := range rec.Joins {
		j, err := wj.Into()
		if err != nil {
			return fmt.Errorf("recorded join %d: %w", i, err)
		}
		if err := form.AddJoin(j); err != nil {
			return fmt.Errorf("recorded join %d: %w", i, err)
		}
	}
	if rec.Bound {
		// Binding again reproduces this seat's own commitment, and the
		// roster it produces has to be the one already committed to. If
		// it is not, this key has said two different things about one
		// session, and the only safe answer is to publish nothing.
		c, err := form.Bind()
		if err != nil {
			return fmt.Errorf("cannot take back up what this key committed to: %w", err)
		}
		if got := hex.EncodeToString(c.Roster[:]); got != rec.Roster {
			return fmt.Errorf(
				"resuming would commit to %s, but this key already committed to %s", got, rec.Roster)
		}
	}
	// Everyone's commit, not only this seat's. The others arrived as
	// messages nobody will send twice, so without them a table that had
	// already seated comes back a signature short and waits for one that is
	// never coming. Each is checked against the terms on the way in, so an
	// edited file is refused rather than believed.
	for i, wc := range rec.Commits {
		c, err := wc.Into()
		if err != nil {
			return fmt.Errorf("recorded commit %d: %w", i, err)
		}
		if err := form.AddCommit(c); err != nil {
			return fmt.Errorf("recorded commit %d: %w", i, err)
		}
	}
	if rec.Beacon != "" {
		beacon, err := hex.DecodeString(rec.Beacon)
		if err != nil {
			return fmt.Errorf("recorded beacon: %w", err)
		}
		if err := form.SetBeacon(beacon); err != nil {
			return fmt.Errorf("recorded beacon: %w", err)
		}
	}

	t.setFormation(form)
	for seat, hexed := range rec.Payouts {
		pay, err := hex.DecodeString(hexed)
		if err != nil {
			return fmt.Errorf("recorded payout for seat %d: %w", seat, err)
		}
		t.payouts[seat] = pay
	}

	if seats, ok := form.Seats(); ok {
		t.seats = seats
	}

	// The game gets its own state back before the table is live, so it is
	// ready before the first message for it arrives.
	if p, ok := r.rules.(Persisting); ok && len(rec.Game) > 0 {
		if err := p.LoadTable(rec.Match, rec.Game); err != nil {
			return fmt.Errorf("the game could not take its own state back up: %w", err)
		}
	}

	r.mu.Lock()
	r.tables[rec.Match] = t
	r.mu.Unlock()

	// Derived rather than stored: this seat's punishment key comes out of
	// the seed and the roster, so it is never written down. Rebuilding the
	// bonds needs every seat's announcement, which is why it is tried and
	// not required.

	return nil
}

// MemTableStore keeps tables in memory. For tests, and for a game that has
// decided it does not want to survive a restart.
type MemTableStore struct {
	mu   sync.Mutex
	recs map[string]TableRecord
	ops  map[string]string
}

func NewMemTableStore() *MemTableStore {
	return &MemTableStore{recs: map[string]TableRecord{}}
}

func (m *MemTableStore) LoadTables() ([]TableRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TableRecord, 0, len(m.recs))
	for _, r := range m.recs {
		out = append(out, cloneRecord(r))
	}
	return out, nil
}

func (m *MemTableStore) SaveTable(rec TableRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs[rec.Match] = cloneRecord(rec)
	return nil
}

func (m *MemTableStore) DropTable(match string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.recs, match)
	return nil
}

// FileTableStore keeps one file per table in a directory.
type FileTableStore struct {
	dir string
	mu  sync.Mutex
}

// NewFileTableStore keeps tables under dir, which is created if it is not
// there.
func NewFileTableStore(dir string) (*FileTableStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("a table store needs a directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create the table directory: %w", err)
	}
	return &FileTableStore{dir: dir}, nil
}

// matchName is what a match id may look like, because it is built into a
// filename. Checked rather than trusted: an id reaches this from an invitation.
var matchName = regexp.MustCompile(`^[0-9a-zA-Z_-]{1,64}$`)

func (f *FileTableStore) path(match string) (string, error) {
	if !matchName.MatchString(match) {
		return "", fmt.Errorf("table %q is not a name this can store", match)
	}
	return filepath.Join(f.dir, match+".json"), nil
}

func (f *FileTableStore) LoadTables() ([]TableRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := os.ReadDir(f.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the table directory: %w", err)
	}
	var out []TableRecord
	var failures []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		blob, err := os.ReadFile(filepath.Join(f.dir, e.Name()))
		if err != nil {
			failures = append(failures, fmt.Errorf("read %s: %w", e.Name(), err))
			continue
		}
		var rec TableRecord
		if err := json.Unmarshal(blob, &rec); err != nil {
			failures = append(failures, fmt.Errorf("read %s: %w", e.Name(), err))
			continue
		}
		if rec.Match+".json" != e.Name() {
			failures = append(failures, fmt.Errorf("record filename mismatch: %s", e.Name()))
			continue
		}
		out = append(out, rec)
	}
	return out, errors.Join(failures...)
}

func (f *FileTableStore) SaveTable(rec TableRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	path, err := f.path(rec.Match)
	if err != nil {
		return err
	}
	blob, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	// Renamed into place, so a run that stops mid-write finds the previous
	// table rather than half of this one - and under a name nothing else
	// will pick, because a restarted daemon can overlap its predecessor on
	// one directory and two writers sharing a temporary name interleave.
	tmp, err := os.CreateTemp(f.dir, rec.Match+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		return fmt.Errorf("write the table: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write the table: %w", err)
	}
	return syncDirectory(f.dir)
}

func (f *FileTableStore) DropTable(match string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	path, err := f.path(match)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("forget the table: %w", err)
	}
	return syncDirectory(f.dir)
}

// Getting up from a table is the game's message, not this one's.
//
// A seat saying it is leaving carries the point in the game it is leaving at,
// and its signature is checked against the game's own log - so the runtime has
// neither the vocabulary to write one nor the standing to check one, and
// claiming the message would only swallow the game's traffic.
//
// What is the runtime's is the money: whether anything is still owed at a
// table, and therefore whether the table may be let go of at all.

// HoldsOurs reports whether this peer still has coin at a table that nothing
// has taken back out.
//
// It is what stands between a table being tidied away and a timelocked stake
// outliving every record of where it is. The lock is measured in days and this
// process is not.
func (r *Runtime) HoldsOurs(match string) bool {
	for _, rec := range r.book.All() {
		if rec.Match == match && rec.State != spend.Denied && rec.State != spend.Expired && rec.State != spend.Located {
			return true
		}
	}
	r.mu.Lock()
	pending := r.tables[match]
	held := pending != nil && pending.seatBond.outpoint != ""
	r.mu.Unlock()
	if held {
		return true
	}
	if pending != nil && pending.formation() == nil && !pending.recoveryOnly {
		return true
	}
	t, seat, err := r.ourSeatAt(match)
	if err != nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, held := range []map[uint32]staked{t.funded} {
		if held[seat].outpoint != "" {
			return true
		}
	}
	return false
}

// Drop forgets a table, on disk and in memory.
//
// Refused while this peer still has coin at it. A table is the only record of
// which outpoint held this seat's stake and which script it can be spent back
// out of; forgetting one that still holds coin does not lose the coin, but it
// does lose the only thing that knows how to reach it.
func (r *Runtime) Drop(match string) error {
	if r.HoldsOurs(match) {
		return fmt.Errorf(
			"table %s still holds coin of ours; settle or reclaim it before letting the table go", match)
	}
	r.mu.Lock()
	_, ok := r.tables[match]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("no table %q", match)
	}
	if r.store != nil {
		if err := r.store.DropTable(match); err != nil {
			return r.storageFault(err)
		}
	}
	r.mu.Lock()
	delete(r.tables, match)
	r.mu.Unlock()
	return nil
}
