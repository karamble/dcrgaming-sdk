package runtime

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
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
	Match string `json:"match"`
	GCID  string `json:"gcid"`

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
	Funded        map[uint32]Funded `json:"funded,omitempty"`
	TableBonds    map[uint32]Funded `json:"tableBonds,omitempty"`
	ForfeitBonds  map[uint32]Funded `json:"forfeitBonds,omitempty"`
	Payouts       map[uint32]string `json:"payouts,omitempty"`
	PunishmentKey map[uint32]string `json:"punishmentKeys,omitempty"`

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
	rec := TableRecord{Match: t.match, GCID: t.gcID, Terms: t.form.Terms()}
	for _, j := range t.form.Joins() {
		rec.Joins = append(rec.Joins, schema.JoinFrom(j))
	}
	for _, c := range t.form.Commits() {
		rec.Commits = append(rec.Commits, schema.CommitFrom(c))
	}
	if b := t.form.Beacon(); len(b) > 0 {
		rec.Beacon = hex.EncodeToString(b)
	}
	if h, ok := t.form.RosterHash(); ok && bound(t.form.State()) {
		rec.Bound, rec.Roster = true, hex.EncodeToString(h[:])
	}
	if t.form.State() == membership.Aborted {
		rec.Aborted, rec.Reason = true, t.form.Reason()
	}

	rec.Funded = fundedOf(t.funded)
	rec.TableBonds = fundedOf(t.tableBondFunded)
	rec.ForfeitBonds = fundedOf(t.forfeitFunded)
	for seat, pay := range t.payouts {
		if rec.Payouts == nil {
			rec.Payouts = map[uint32]string{}
		}
		rec.Payouts[seat] = hex.EncodeToString(pay)
	}
	for seat, pub := range t.punishPubs {
		if rec.PunishmentKey == nil {
			rec.PunishmentKey = map[uint32]string{}
		}
		rec.PunishmentKey[seat] = hex.EncodeToString(pub)
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
// Called after anything that changes a table durably. Failing to write is
// logged rather than returned: the change has already happened in memory and
// often on the chain, and unwinding it because a disk was full would be worse
// than coming back up a record short.
func (r *Runtime) keep(t *table) {
	if r.store == nil {
		return
	}
	r.write(t, r.snapshot(t))
}

// write persists one rendered record. Never called under r.mu: it calls into
// the game, and a game that called back would find the runtime holding its own
// lock.
func (r *Runtime) write(t *table, rec TableRecord) {
	if p, ok := r.rules.(Persisting); ok {
		blob, err := p.SaveTable(t.match)
		if err != nil {
			r.log.Errorf("table %s: the game could not save its own state: %v", t.match, err)
		} else {
			rec.Game = blob
		}
	}
	if err := r.store.SaveTable(rec); err != nil {
		r.log.Errorf("table %s: could not write it down: %v", t.match, err)
	}
}

// forget drops a table from the store, for one that is over.
func (r *Runtime) forget(match string) {
	if r.store == nil {
		return
	}
	if err := r.store.DropTable(match); err != nil {
		r.log.Errorf("table %s: could not forget it: %v", match, err)
	}
}

// Resume takes back up every table that was written down.
//
// Called before Run. A table that will not resume is reported and skipped
// rather than taken down with the rest: one unreadable record must not cost
// every other table its settlement.
func (r *Runtime) Resume() error {
	if r.store == nil {
		return nil
	}
	recs, err := r.store.LoadTables()
	if err != nil {
		return fmt.Errorf("read the tables back: %w", err)
	}
	// In a fixed order, so a run that resumes ten tables logs the same way
	// twice and a failure can be found again.
	sort.Slice(recs, func(i, j int) bool { return recs[i].Match < recs[j].Match })

	var failed int
	for _, rec := range recs {
		if err := r.resume(rec); err != nil {
			failed++
			r.log.Errorf("table %s did not come back: %v", rec.Match, err)
		}
	}
	if failed > 0 {
		r.log.Warnf("%d of %d tables did not come back", failed, len(recs))
	}
	return nil
}

// resume rebuilds one table from its record.
func (r *Runtime) resume(rec TableRecord) error {
	if rec.Match == "" {
		return fmt.Errorf("a record with no table")
	}
	if rec.Aborted {
		// Terminal, and it stays terminal. Rebuilt as a tombstone
		// rather than a table so a commit that arrives after everyone
		// else gave up cannot put this process back into a membership
		// nobody is bound to.
		r.mu.Lock()
		r.ended[rec.Match] = rec.Reason
		r.mu.Unlock()
		return nil
	}
	creds, err := r.seatCredentials(rec.Terms)
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

	t := &table{
		match: rec.Match, gcID: rec.GCID, form: form,
		funded:          stakedOf(rec.Funded),
		tableBondFunded: stakedOf(rec.TableBonds),
		forfeitFunded:   stakedOf(rec.ForfeitBonds),
		payouts:         map[uint32][]byte{},
		punishPubs:      map[uint32][]byte{},
	}
	for seat, hexed := range rec.Payouts {
		pay, err := hex.DecodeString(hexed)
		if err != nil {
			return fmt.Errorf("recorded payout for seat %d: %w", seat, err)
		}
		t.payouts[seat] = pay
	}
	for seat, hexed := range rec.PunishmentKey {
		pub, err := hex.DecodeString(hexed)
		if err != nil {
			return fmt.Errorf("recorded punishment key for seat %d: %w", seat, err)
		}
		t.punishPubs[seat] = pub
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
	if len(t.punishPubs) > 0 {
		if seats, ok := form.Seats(); ok {
			if mine, ours := form.OurSeat(); ours {
				if key, err := r.punishKeyFor(t, seats, mine); err == nil {
					r.mu.Lock()
					t.punish = key
					r.mu.Unlock()
				}
			}
			if len(t.punishPubs) == len(seats) {
				if err := r.buildForfeitableBonds(t); err != nil {
					r.log.Warnf("table %s: forfeitable bonds did not come back: %v", rec.Match, err)
				}
			}
		}
	}
	return nil
}

// MemTableStore keeps tables in memory. For tests, and for a game that has
// decided it does not want to survive a restart.
type MemTableStore struct {
	mu   sync.Mutex
	recs map[string]TableRecord
}

func NewMemTableStore() *MemTableStore {
	return &MemTableStore{recs: map[string]TableRecord{}}
}

func (m *MemTableStore) LoadTables() ([]TableRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TableRecord, 0, len(m.recs))
	for _, r := range m.recs {
		out = append(out, r)
	}
	return out, nil
}

func (m *MemTableStore) SaveTable(rec TableRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs[rec.Match] = rec
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
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		blob, err := os.ReadFile(filepath.Join(f.dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		var rec TableRecord
		if err := json.Unmarshal(blob, &rec); err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		out = append(out, rec)
	}
	return out, nil
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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return fmt.Errorf("write the table: %w", err)
	}
	// Renamed into place, so a run that stops mid-write finds the previous
	// table rather than half of this one.
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write the table: %w", err)
	}
	return nil
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
	return nil
}
