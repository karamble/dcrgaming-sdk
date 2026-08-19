package spend

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
)

func unreachableErr() error { return status.Error(codes.Unavailable, "the bridge is unreachable") }

func book(t *testing.T) *Book {
	t.Helper()
	b, err := OpenBook(MemStore())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return b
}

// asked writes a request down and gives it the bridge's id, which is the state
// a request is in the moment the bridge has acknowledged it.
func asked(t *testing.T, b *Book, id string) Record {
	t.Helper()
	r := Record{
		ID: id, Match: "m1", Seat: 1, Purpose: "stake",
		Address: "Tsabc", Atoms: 5_000_000, PkScript: "76a914",
	}
	if err := b.Put(r); err != nil {
		t.Fatalf("put: %v", err)
	}
	return r
}

// THE test of this package. A bridge that cannot be reached must leave the
// request open, because the payment may already have been made.
func TestAnUnreachableBridgeLeavesTheRequestOpen(t *testing.T) {
	b := book(t)
	asked(t, b, "s1")

	got, err := b.Note("s1", transport.Spend{}, unreachableErr())
	if err != nil {
		t.Fatalf("note: %v", err)
	}
	if got.State != Unknown {
		t.Fatalf("state is %q, want %q", got.State, Unknown)
	}
	if !got.State.Open() {
		t.Fatal("a request nobody could ask about was closed; the payment may have been made")
	}
	if got.Unreachable == "" {
		t.Fatal("the reason this could not ask was not kept")
	}
	if got.Error != "" {
		t.Fatalf("an unreachable bridge was recorded as a refusal: %q", got.Error)
	}
}

// And it must still be open after a restart, because a restart of either side
// is exactly when this happens.
func TestAnUnknownRequestIsStillOpenAfterARestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spends.json")
	store, err := FileStore(path)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	b, err := OpenBook(store)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	asked(t, b, "s1")
	if _, err := b.Note("s1", transport.Spend{}, unreachableErr()); err != nil {
		t.Fatalf("note: %v", err)
	}

	reopened, err := FileStore(path)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	b2, err := OpenBook(reopened)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	open := b2.OpenRecords()
	if len(open) != 1 || open[0].ID != "s1" || open[0].State != Unknown {
		t.Fatalf("after a restart the open set is %+v", open)
	}
}

// An approved payment stays approved. Not being able to ask again does not
// unsay what we were already told.
func TestAnUnreachableBridgeDoesNotUnsayAnApproval(t *testing.T) {
	b := book(t)
	asked(t, b, "s1")
	if _, err := b.Note("s1", transport.Spend{State: transport.SpendApproved, TxID: "abc"}, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, err := b.Note("s1", transport.Spend{}, unreachableErr())
	if err != nil {
		t.Fatalf("note: %v", err)
	}
	if got.State != Approved {
		t.Fatalf("an approved payment became %q when the bridge went away", got.State)
	}
	if got.TxID != "abc" {
		t.Fatalf("the txid was lost: %q", got.TxID)
	}
}

// A refusal is the other thing entirely: the bridge answered, and it is over.
func TestARefusalIsTerminalAndKeepsItsReason(t *testing.T) {
	b := book(t)
	asked(t, b, "s1")
	got, err := b.Note("s1", transport.Spend{State: transport.SpendDenied, Error: "over the cap"}, nil)
	if err != nil {
		t.Fatalf("note: %v", err)
	}
	if got.State != Denied || got.State.Open() {
		t.Fatalf("a refusal left the request %q, open=%v", got.State, got.State.Open())
	}
	if got.Error != "over the cap" {
		t.Fatalf("the refusal lost its reason: %q", got.Error)
	}
}

func TestNothingLeavesATerminalState(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  transport.SpendState
	}{
		{"denied", transport.SpendDenied},
		{"failed", transport.SpendFailed},
		{"expired", transport.SpendExpired},
	} {
		b := book(t)
		asked(t, b, "s1")
		if _, err := b.Note("s1", transport.Spend{State: tc.end}, nil); err != nil {
			t.Fatalf("%s: note: %v", tc.name, err)
		}
		got, err := b.Note("s1", transport.Spend{State: transport.SpendApproved, TxID: "abc"}, nil)
		if err != nil {
			t.Fatalf("%s: second note: %v", tc.name, err)
		}
		if got.State == Approved {
			t.Errorf("%s: a settled request was reopened as approved, which pays twice", tc.name)
		}
		if got.TxID != "" {
			t.Errorf("%s: a settled request gained a txid", tc.name)
		}
	}
}

func TestAPaymentIsOnlyLocatedFromApproved(t *testing.T) {
	b := book(t)
	asked(t, b, "s1")
	if _, err := b.Locate("s1", "abc:1"); err == nil {
		t.Fatal("located an output for a payment nobody had approved")
	}
	if _, err := b.Note("s1", transport.Spend{State: transport.SpendApproved, TxID: "abc"}, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	got, err := b.Locate("s1", "abc:1")
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	if got.State != Located || got.Outpoint != "abc:1" {
		t.Fatalf("after locating: state=%q outpoint=%q", got.State, got.Outpoint)
	}
	if got.State.Open() {
		t.Fatal("a located payment is still being watched")
	}
}

// An error this package does not understand decides nothing. Guessing is how a
// transport hiccup becomes a refusal.
func TestAnUnrecognisedErrorDecidesNothing(t *testing.T) {
	b := book(t)
	asked(t, b, "s1")
	boom := errors.New("something else went wrong")
	got, err := b.Note("s1", transport.Spend{}, boom)
	if !errors.Is(err, boom) {
		t.Fatalf("the error was swallowed or changed: %v", err)
	}
	if got.State != Requested {
		t.Fatalf("an unrecognised error moved the request to %q", got.State)
	}
}

// The bridge saying something this does not recognise is also not an answer.
func TestAnUnrecognisedAnswerIsRefused(t *testing.T) {
	b := book(t)
	asked(t, b, "s1")
	if _, err := b.Note("s1", transport.Spend{State: transport.SpendState("sideways")}, nil); err == nil {
		t.Fatal("accepted an answer with no meaning")
	}
	if got, _ := b.Get("s1"); got.State != Requested {
		t.Fatalf("an unrecognised answer moved the request to %q", got.State)
	}
}

// The book is on disk before any watcher runs, so a game mirroring it can never
// be ahead of the record it mirrors.
func TestTheBookIsPersistedBeforeAWatcherSeesIt(t *testing.T) {
	store := MemStore()
	b, err := OpenBook(store)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var sawOnDisk bool
	b.Watch(func(r Record) {
		rs, err := store.Load()
		if err != nil {
			t.Errorf("load inside watcher: %v", err)
			return
		}
		for _, s := range rs {
			if s.ID == r.ID && s.State == r.State {
				sawOnDisk = true
			}
		}
	})
	asked(t, b, "s1")
	if !sawOnDisk {
		t.Fatal("a watcher ran before the change was stored")
	}
}

func TestAnUnreadableBookIsRefusedRatherThanStartedEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spends.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	store, err := FileStore(path)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, err := OpenBook(store); err == nil {
		t.Fatal("a corrupt book started empty, silently forgetting every payment it held")
	}
}

func TestTheBookOnDiskIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spends.json")
	store, err := FileStore(path)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	b, err := OpenBook(store)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	asked(t, b, "s1")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the spend book is mode %o, want 600", perm)
	}
}

func TestARequestMustSayEnoughToBeAnswered(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Record
	}{
		{"no table", Record{ID: "s1", Atoms: 1, Address: "T"}},
		{"nothing to pay", Record{ID: "s1", Match: "m", Address: "T"}},
		{"a negative amount", Record{ID: "s1", Match: "m", Atoms: -1, Address: "T"}},
		{"nowhere to pay", Record{ID: "s1", Match: "m", Atoms: 1}},
		{"a state that does not exist", Record{ID: "s1", Match: "m", Atoms: 1, Address: "T", State: "sideways"}},
	} {
		if err := book(t).Put(tc.r); err == nil {
			t.Errorf("%s: accepted a request that could not be answered", tc.name)
		}
	}
}

// A request written down before the bridge issued an id is still a request, and
// must be findable afterwards - otherwise a crash between the two loses it.
func TestARequestWrittenBeforeItsIdIsAdoptedLater(t *testing.T) {
	b := book(t)
	if err := b.Put(Record{Match: "m1", Purpose: "bond", Address: "Tsabc", Atoms: 1_000_000}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if open := b.OpenRecords(); len(open) != 1 || open[0].ID != "" {
		t.Fatalf("the unidentified request is not open: %+v", open)
	}
	got, err := b.Adopt("m1", "bond", "s7")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if got.ID != "s7" {
		t.Fatalf("adopted as %q", got.ID)
	}
	if _, ok := b.Get("s7"); !ok {
		t.Fatal("the adopted request cannot be found by its id")
	}
	if open := b.OpenRecords(); len(open) != 1 {
		t.Fatalf("adoption duplicated the request: %+v", open)
	}
}

// Both games' records have to fit the one type, which is the point of unifying
// them: poker keyed by session and seat, battleships by match.
func TestBothGamesRecordsFitOneType(t *testing.T) {
	for _, tc := range []struct {
		game string
		r    Record
	}{
		{"poker", Record{ID: "s1", Match: "sid-abc", Seat: 3, Purpose: "stake", Address: "Ts1", Atoms: 5_000_000, PkScript: "76a914"}},
		{"battleships", Record{ID: "s2", Match: "sid-def", Purpose: "seatbond", Address: "Ts2", Atoms: 1_000_000, PkScript: "a914"}},
	} {
		if err := book(t).Put(tc.r); err != nil {
			t.Errorf("%s: %v", tc.game, err)
		}
	}
}

// A record names an address and an amount and never a script it built or a
// transaction it made up. This pins the shape.
func TestARecordNamesNoTransactionItBuilt(t *testing.T) {
	want := map[string]bool{
		"ID": true, "Match": true, "Seat": true, "Purpose": true,
		"Address": true, "Atoms": true, "PkScript": true, "State": true,
		"TxID": true, "Outpoint": true, "Error": true, "Unreachable": true,
	}
	got := recordFields()
	if len(got) != len(want) {
		t.Fatalf("Record has %d fields, want %d: %v", len(got), len(want), got)
	}
	for _, f := range got {
		if !want[f] {
			t.Errorf("Record gained a field %q; if it names a raw transaction or a "+
				"script this built, it does not belong here", f)
		}
	}
}

func recordFields() []string {
	typ := reflect.TypeOf(Record{})
	out := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		out = append(out, typ.Field(i).Name)
	}
	return out
}

// The rule table itself, tested directly rather than through whichever caller
// happens to reach each branch today. Note deliberately returns a settled
// request unchanged instead of erroring, so its terminal rule is not otherwise
// exercised - and a rule nothing tests is a rule that quietly stops holding.
func TestTheStateMachineRules(t *testing.T) {
	for _, tc := range []struct {
		from, to State
		ok       bool
	}{
		{Requested, Unknown, true},
		{Requested, Approved, true},
		{Requested, Denied, true},
		{Unknown, Approved, true},
		{Unknown, Denied, true},
		{Unknown, Requested, true},
		{Approved, Located, true},
		{Denied, Denied, true},

		{Denied, Approved, false},
		{Denied, Requested, false},
		{Denied, Unknown, false},
		{Failed, Approved, false},
		{Expired, Approved, false},
		{Located, Approved, false},
		{Located, Unknown, false},

		{Requested, Located, false},
		{Unknown, Located, false},
		{Denied, Located, false},

		{Requested, State("sideways"), false},
	} {
		err := canFollow(tc.from, tc.to)
		if tc.ok && err != nil {
			t.Errorf("%s -> %s was refused: %v", tc.from, tc.to, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s -> %s was allowed", tc.from, tc.to)
		}
	}
}

// Two writers on one file do not interleave into one record.
//
// A restarted daemon can overlap its predecessor on the same directory for a
// moment. With one fixed temporary name they write into the same file and
// rename it in turn, and what lands is half of each - a money record that
// describes neither process's idea of what is owed.
func TestTwoWritersDoNotInterleaveTheMoneyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spends.json")
	one, err := FileStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	two, err := FileStore(path)
	if err != nil {
		t.Fatalf("open again: %v", err)
	}

	// Different sizes, so a half-written file is not accidentally valid.
	small := []Record{{ID: "a", Match: "m", Purpose: "stake", Address: "Ts", Atoms: 1}}
	big := make([]Record, 0, 200)
	for i := range 200 {
		big = append(big, Record{
			ID: fmt.Sprintf("b%d", i), Match: "m", Purpose: "stake",
			Address: strings.Repeat("T", 64), Atoms: int64(i),
		})
	}

	var wg sync.WaitGroup
	var failed sync.Map
	save := func(st Store, rs []Record, who string) {
		defer wg.Done()
		if err := st.Save(rs); err != nil {
			failed.Store(who, err)
		}
	}
	for range 40 {
		wg.Add(2)
		go save(one, small, "one")
		go save(two, big, "two")
	}
	wg.Wait()

	// Nobody's write failed. With one temporary name shared between them,
	// each writer's cleanup deletes the file the other is about to rename.
	failed.Range(func(who, err any) bool {
		t.Errorf("writer %v could not save: %v", who, err)
		return true
	})

	// Whatever landed has to be one of the two, whole.
	back, err := FileStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := back.Load()
	if err != nil {
		t.Fatalf("the money file is unreadable after concurrent writes: %v", err)
	}
	if len(got) != len(small) && len(got) != len(big) {
		t.Fatalf("the file holds %d records, which is neither writer's %d nor %d",
			len(got), len(small), len(big))
	}
	// And no temporary files left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}
