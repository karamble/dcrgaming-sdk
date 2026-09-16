package spend

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
)

// Record is one request to move money, and everything learned about it since.
//
// It names an address and an amount. It never names a script it built or a
// transaction it made up: the bridge decides what to sign and escrow decides
// what a script says, and a record that could name either would be a record
// that could redirect a payment.
type Record struct {
	DepositID string `json:",omitempty"` // immutable descriptor retained by the bridge
	// ID is the bridge's id for the request. Empty until the bridge answers
	// the first ask, because the id is the bridge's to issue.
	ID string
	// Obligation binds a request to immutable game terms and derived identity.
	Obligation string `json:",omitempty"`
	Attempt    uint32 `json:",omitempty"`

	// Match and Seat say which table and seat the money is for. Both games
	// needed both; one keyed by session id and seat, the other by match, and
	// the mismatch is why neither could read the other's book.
	Match string
	Seat  uint32

	// Purpose is the game's own word for what the money is - a stake, a
	// bond, a buy-in. Carried, never interpreted.
	Purpose string

	// Address is where the bridge was asked to pay, and Atoms how much.
	Address string
	Atoms   int64

	// PkScript is the script the payment must land in, hex. Kept so a
	// resumed request can find its output without re-deriving anything.
	PkScript string

	// State is where the request stands. See the package comment.
	State State

	// TxID is set once the bridge approves, and Outpoint once the output is
	// found. Outpoint is "txid:vout", one field rather than two, because
	// half an outpoint is not a thing a caller should be able to hold.
	TxID     string
	Outpoint string

	// Error is the bridge's own reason for refusing. Reachable is why this
	// process last failed to ask. They are separate fields because they are
	// separate facts, and the whole package exists to keep them apart.
	Error       string
	Unreachable string
}

// Store is where a book is kept between runs.
//
// The default is [FileStore] and it is what a game gets unless it says
// otherwise. Implementing this yourself is supported and moves the correctness
// of the money record to you: a Save that loses a record loses a payment nobody
// is watching any more.
type Store interface {
	Load() ([]Record, error)
	Save([]Record) error
}

// Book is the persistent record of every request a game has made.
//
// Its mutex sits at the bottom of the lock hierarchy: nothing else is held
// while it is.
type Book struct {
	mu      sync.Mutex
	store   Store
	records map[string]Record
	pending []Record // requested before the bridge issued an id
	watch   []func(Record)
	fault   error
}

// OpenBook reads a book back, or starts an empty one.
func OpenBook(store Store) (*Book, error) {
	if store == nil {
		return nil, fmt.Errorf("a book needs somewhere to be kept")
	}
	rs, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("read the spend book: %w", err)
	}
	b := &Book{store: store, records: make(map[string]Record, len(rs))}
	for _, r := range rs {
		if r.ID == "" {
			b.pending = append(b.pending, r)
			continue
		}
		if _, exists := b.records[r.ID]; exists {
			return nil, fmt.Errorf("duplicate payment request id %q", r.ID)
		}
		b.records[r.ID] = r
	}
	return b, nil
}

// Watch registers a function called with every record that changes, after it is
// on disk.
//
// This is how a game keeps its own database in step without owning the money
// record: the book has already persisted the change by the time this runs, so a
// watcher that fails loses the game's copy and not the book's.
func (b *Book) Watch(f func(Record)) {
	b.mu.Lock()
	b.watch = append(b.watch, f)
	b.mu.Unlock()
}

// Put writes a record down before anything is asked of the bridge.
//
// Written first on purpose: a request the bridge received but this process
// never recorded is a payment nobody is watching. Better a record of something
// that never happened, which is discovered by asking, than a payment with no
// record, which is discovered by an accountant.
func (b *Book) Put(r Record) error {
	if r.Match == "" {
		return fmt.Errorf("a request must say which table it is for")
	}
	if r.Atoms <= 0 {
		return fmt.Errorf("a request for %d atoms is not a request", r.Atoms)
	}
	if strings.TrimSpace(r.Address) == "" {
		return fmt.Errorf("a request must say where to pay")
	}
	if r.State == "" {
		r.State = Requested
	}
	if !r.State.valid() {
		return fmt.Errorf("%q is not a state a request can be in", r.State)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.commitLocked(r)
}

// Note records what the bridge said, or that it could not be asked.
//
// Exactly one of spend and err is meaningful. An err that transport.Unreachable
// recognises moves the request to Unknown and leaves it open; any other error is
// returned untouched, because an error this package does not understand is not
// grounds to decide anything about money.
func (b *Book) Note(id string, sp transport.Spend, err error) (Record, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	r, ok := b.records[id]
	if !ok {
		return Record{}, fmt.Errorf("no request is recorded as %q", id)
	}
	if r.State.Terminal() {
		return r, nil
	}

	if err != nil {
		if !transport.Unreachable(err) {
			return r, err
		}
		// Could not ask. Not an answer: the payment may have been made.
		// An already-approved payment keeps its state, because approval
		// is something we were told and not being able to ask again does
		// not unsay it.
		r.Unreachable = err.Error()
		if r.State != Approved {
			if fErr := canFollow(r.State, Unknown); fErr != nil {
				return r, fErr
			}
			r.State = Unknown
		}
		return r, b.commitLocked(r)
	}

	r.Unreachable = ""
	next, reason := fromTransport(sp.State)
	if next == "" {
		return r, fmt.Errorf("the bridge answered %q, which is not an answer this knows", sp.State)
	}
	if fErr := canFollow(r.State, next); fErr != nil {
		return r, fErr
	}
	r.State = next
	if sp.TxID != "" {
		r.TxID = sp.TxID
	}
	if reason {
		r.Error = sp.Error
	}
	return r, b.commitLocked(r)
}

// Locate records that an approved payment's output has been found.
func (b *Book) Locate(id, outpoint string) (Record, error) {
	if strings.TrimSpace(outpoint) == "" {
		return Record{}, fmt.Errorf("locating a payment needs the outpoint it landed in")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.records[id]
	if !ok {
		return Record{}, fmt.Errorf("no request is recorded as %q", id)
	}
	if err := canFollow(r.State, Located); err != nil {
		return r, err
	}
	r.State, r.Outpoint = Located, outpoint
	return r, b.commitLocked(r)
}

// Adopt attaches the id the bridge issued to a request that was written down
// before it had one.
func (b *Book) Adopt(match, purpose, id string) (Record, error) {
	if id == "" {
		return Record{}, fmt.Errorf("adopting a request needs the id the bridge issued")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.records[id]; exists {
		return Record{}, fmt.Errorf("request id already belongs to an obligation")
	}
	found := -1
	for i, p := range b.pending {
		if p.Match == match && p.Purpose == purpose {
			if found >= 0 {
				return Record{}, fmt.Errorf("ambiguous requests require reconciliation")
			}
			found = i
		}
	}
	if found >= 0 {
		p := b.pending[found]
		p.ID = id
		pending := append([]Record(nil), b.pending[:found]...)
		pending = append(pending, b.pending[found+1:]...)
		records := make(map[string]Record, len(b.records)+1)
		for key, old := range b.records {
			records[key] = old
		}
		records[id] = p
		return p, b.saveLocked(records, pending, p)
	}

	return Record{}, fmt.Errorf("no unidentified %s request is recorded for %q", purpose, match)
}

// Get reads one record back.
func (b *Book) Get(id string) (Record, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.records[id]
	return r, ok
}

// OpenRecords returns every request that may still move money, which is what a
// game asks for after a restart. Sorted by id so a resume is deterministic.
func (b *Book) OpenRecords() []Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Record
	for _, r := range b.records {
		if r.State.Open() {
			out = append(out, r)
		}
	}
	out = append(out, b.pending...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// All returns every record, open or not, sorted by id.
func (b *Book) All() []Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.allLocked()
}

func (b *Book) allLocked() []Record {
	out := make([]Record, 0, len(b.records)+len(b.pending))
	for _, r := range b.records {
		out = append(out, r)
	}
	out = append(out, b.pending...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// commitLocked persists one change and tells the watchers. Caller holds the
// lock. The disk write happens before any watcher runs, so a game mirroring the
// book can never be ahead of it.
func (b *Book) commitLocked(r Record) error {
	records := make(map[string]Record, len(b.records)+1)
	for id, old := range b.records {
		records[id] = old
	}
	pending := append([]Record(nil), b.pending...)
	if r.ID == "" {
		pending = append(pending, r)
	} else {
		records[r.ID] = r
	}
	return b.saveLocked(records, pending, r)
}

func (b *Book) saveLocked(records map[string]Record, pending []Record, changed Record) error {
	if b.fault != nil {
		return b.fault
	}
	all := append([]Record(nil), pending...)
	for _, r := range records {
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	if err := b.store.Save(all); err != nil {
		b.fault = fmt.Errorf("write the spend book: %w", err)
		return b.fault
	}
	b.records, b.pending = records, pending
	for _, f := range b.watch {
		f(changed)
	}
	return nil
}

// fromTransport maps the bridge's answer onto a state, and says whether the
// answer carries a reason worth keeping.
func fromTransport(s transport.SpendState) (State, bool) {
	switch s {
	case transport.SpendPending:
		return Requested, false
	case transport.SpendApproved:
		return Approved, false
	case transport.SpendDenied:
		return Denied, true
	case transport.SpendFailed:
		return Failed, true
	case transport.SpendExpired:
		return Expired, true
	}
	return "", false
}

// Err reports a latched persistence fault. Reopen from disk after repairing storage.
func (b *Book) Err() error { b.mu.Lock(); defer b.mu.Unlock(); return b.fault }
