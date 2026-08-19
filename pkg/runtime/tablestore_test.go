package runtime

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/connect"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
	"github.com/karamble/dcrgaming-sdk/pkg/identity"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

// standAt is stand with a seed directory and a table store of the caller's
// choosing, so a second runtime can be brought up on the first one's state -
// which is what a restart is.
func standAt(t *testing.T, g Rules, seedDir string, store TableStore) (*bridgetest.Bridge, *Runtime) {
	t.Helper()
	fake := bridgetest.New(bridgetest.Options{
		Game: g.Identity().GameID, Network: "mainnet",
		Params: chaincfg.TestNet3Params(), Height: 1000,
	})
	srv, err := fake.Serve("seat0")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	id := g.Identity()
	conn, err := srv.Dial(ctx, "seat0", func(cfg *transport.BridgeConfig) { connect.Stamp(cfg, id) })
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	book, err := spend.OpenBook(spend.MemStore())
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	seed, err := identity.Load(seedDir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if err := seed.SetBondDeposit(bondOutpoint); err != nil {
		t.Fatalf("bond deposit: %v", err)
	}
	rt, err := New(Config{
		Rules: g, Bridge: conn, Book: book, Tables: store,
		Identity: seed, SeatTags: testTags, Params: chaincfg.TestNet3Params(),
		PunishTag: []byte("testgame/punishkey/v1"),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return fake, rt
}

// A seated table comes back seated. Without this a restart forgets a stake
// nobody has settled and a bond nobody has released - which is the whole
// reason both existing games wrote this themselves.
func TestASeatedTableComesBackAfterARestart(t *testing.T) {
	dir, store := t.TempDir(), NewMemTableStore()
	fake, rt := standAt(t, &trivialGame{}, dir, store)
	sid, them := seatTwo(t, fake, rt)

	rt.mu.Lock()
	before := rt.tables[sid]
	rt.mu.Unlock()
	wantHash, err := before.form.Terms().Hash()
	if err != nil {
		t.Fatalf("terms hash: %v", err)
	}
	wantSeats, _ := before.form.Seats()
	wantRoster, _ := before.form.RosterHash()

	// Announce, so the punishment keys are on the record too.
	if err := rt.adoptPunishKey(context.Background(), sid, theirPunishNote(t, rt, sid, them)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	// Where the money went.
	rt.mu.Lock()
	before.funded = map[uint32]staked{0: {outpoint: bondOutpoint, atoms: 5_000_000}}
	rt.mu.Unlock()
	rt.keep(before)

	// The restart: a second runtime on the same seed and the same store.
	_, again := standAt(t, &trivialGame{}, dir, store)
	if err := again.Resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	again.mu.Lock()
	after, ok := again.tables[sid]
	again.mu.Unlock()
	if !ok {
		t.Fatal("the table did not come back at all")
	}
	got, err := after.form.Terms().Hash()
	if err != nil {
		t.Fatalf("terms hash: %v", err)
	}
	if got != wantHash {
		t.Fatalf("the terms came back different:\n got %x\nwant %x", got, wantHash)
	}
	gotSeats, seated := after.form.Seats()
	if !seated {
		t.Fatal("the table came back unseated")
	}
	for seat, key := range wantSeats {
		if hex.EncodeToString(gotSeats[seat]) != hex.EncodeToString(key) {
			t.Fatalf("seat %d came back as somebody else", seat)
		}
	}
	if got, _ := after.form.RosterHash(); got != wantRoster {
		t.Fatalf("the roster came back as %x, not %x", got, wantRoster)
	}
	if before.form.State() != after.form.State() {
		t.Fatalf("the table came back %v, having been %v", after.form.State(), before.form.State())
	}
	if out, atoms, ok := again.Funded(sid, 0); !ok || out != bondOutpoint || atoms != 5_000_000 {
		t.Fatalf("the stake came back as %q/%d/%v", out, atoms, ok)
	}
	// Derived, never stored: the punishment key is back out of the seed.
	if _, ok := again.ForfeitableBond(sid, 0); !ok {
		t.Error("the forfeitable bonds did not come back")
	}
	again.mu.Lock()
	hasKey := after.punish != nil
	again.mu.Unlock()
	if !hasKey {
		t.Error("this seat's own punishment key did not come back")
	}
}

// No key is ever written down. A stored table is a file on a disk that gets
// backed up, copied and read; a seat key in it would be spendable by whoever
// held the copy.
func TestATableRecordHoldsNoKeys(t *testing.T) {
	dir, store := t.TempDir(), NewMemTableStore()
	fake, rt := standAt(t, &trivialGame{}, dir, store)
	sid, them := seatTwo(t, fake, rt)
	if err := rt.adoptPunishKey(context.Background(), sid, theirPunishNote(t, rt, sid, them)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	rt.mu.Lock()
	tbl := rt.tables[sid]
	rt.mu.Unlock()
	rt.keep(tbl)

	recs, err := store.LoadTables()
	if err != nil || len(recs) != 1 {
		t.Fatalf("load: %v %d", err, len(recs))
	}
	blob, err := json.Marshal(recs[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The private keys this table has, none of which may appear.
	rt.mu.Lock()
	secrets := [][]byte{tbl.punish.Serialize()}
	rt.mu.Unlock()
	session, logKey, err := rt.seatKeys(tbl.form.Terms().SID)
	if err != nil {
		t.Fatalf("seat keys: %v", err)
	}
	secrets = append(secrets, session.Serialize(), logKey.Serialize())
	for _, secret := range secrets {
		if strings.Contains(strings.ToLower(string(blob)), hex.EncodeToString(secret)) {
			t.Fatal("a private key was written into the table record")
		}
	}
}

// A table with bond terms comes back with them. The wire's rendering of terms
// leaves the bond fields out, so a record built from it would hash to something
// else and refuse every commit the table had already collected.
func TestBondTermsSurviveARestart(t *testing.T) {
	dir, store := t.TempDir(), NewMemTableStore()
	_, rt := standAt(t, &trivialGame{}, dir, store)
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	rt.mu.Lock()
	want := rt.tables[sid].form.Terms()
	rt.mu.Unlock()
	if want.BondAtoms == 0 {
		t.Fatal("this game states no bond, so the test proves nothing")
	}

	_, again := standAt(t, &trivialGame{}, dir, store)
	if err := again.Resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	again.mu.Lock()
	got := again.tables[sid].form.Terms()
	again.mu.Unlock()
	if got.BondAtoms != want.BondAtoms || got.BondLockBlocks != want.BondLockBlocks {
		t.Errorf("the bond terms came back as %d/%d, not %d/%d",
			got.BondAtoms, got.BondLockBlocks, want.BondAtoms, want.BondLockBlocks)
	}
	gotHash, err := got.Hash()
	if err != nil {
		t.Fatalf("terms hash: %v", err)
	}
	wantHash, err := want.Hash()
	if err != nil {
		t.Fatalf("terms hash: %v", err)
	}
	if gotHash != wantHash {
		t.Errorf("the terms hash moved across a restart:\n got %x\nwant %x", gotHash, wantHash)
	}
}

// An aborted table stays aborted. It comes back as a tombstone rather than a
// table, so a commit that arrives after everyone else gave up cannot put this
// process back into a membership nobody is bound to.
func TestAnAbortedTableComesBackAsATombstone(t *testing.T) {
	dir, store := t.TempDir(), NewMemTableStore()
	_, rt := standAt(t, &trivialGame{}, dir, store)
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	recs, _ := store.LoadTables()
	if len(recs) != 1 {
		t.Fatalf("the table was not written down: %d records", len(recs))
	}
	rec := recs[0]
	rec.Aborted, rec.Reason = true, "only 1 of 2 seats were taken before the deadline"
	if err := store.SaveTable(rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	_, again := standAt(t, &trivialGame{}, dir, store)
	if err := again.Resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	again.mu.Lock()
	_, live := again.tables[sid]
	again.mu.Unlock()
	if live {
		t.Fatal("an aborted table came back live")
	}
	// And it cannot be joined again.
	_, err = accept(again, invite(t, nil), testGCID)
	if err == nil {
		t.Fatal("an aborted session was joined again")
	}
	if !strings.Contains(err.Error(), "already ended") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// Resuming refuses a roster this key did not commit to. Binding again has to
// reproduce the same roster; if it does not, this key has said two different
// things about one session and the only safe answer is to publish nothing.
func TestResumingRefusesARosterThisKeyDidNotCommitTo(t *testing.T) {
	dir, store := t.TempDir(), NewMemTableStore()
	fake, rt := standAt(t, &trivialGame{}, dir, store)
	sid, _ := seatTwo(t, fake, rt)
	rt.mu.Lock()
	tbl := rt.tables[sid]
	rt.mu.Unlock()
	rt.keep(tbl)

	recs, _ := store.LoadTables()
	rec := recs[0]
	if len(rec.Joins) != 2 {
		t.Fatalf("the record holds %d joins, so binding proves nothing", len(rec.Joins))
	}
	// A record claiming this key committed to a roster it never signed.
	// Binding again reproduces the real one, and the two disagree.
	rec.Bound, rec.Roster = true, strings.Repeat("ab", 32)
	if err := store.SaveTable(rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	_, again := standAt(t, &trivialGame{}, dir, store)
	if err := again.resume(rec); err == nil {
		t.Fatal("a table resumed into a roster this key never committed to")
	} else if !strings.Contains(err.Error(), "already committed to") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	// And it is not live either way.
	again.mu.Lock()
	_, live := again.tables[sid]
	again.mu.Unlock()
	if live {
		t.Fatal("the table went live despite being refused")
	}
}

// One unreadable record must not cost every other table its settlement.
func TestOneBadRecordDoesNotCostTheOthers(t *testing.T) {
	dir, store := t.TempDir(), NewMemTableStore()
	_, rt := standAt(t, &trivialGame{}, dir, store)
	if _, err := accept(rt, invite(t, nil), testGCID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	// A record that cannot possibly rebuild, sorting before the good one.
	if err := store.SaveTable(TableRecord{
		Match: "aaaaaaa1", GCID: testGCID, Beacon: "not hex at all",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	_, again := standAt(t, &trivialGame{}, dir, store)
	if err := again.Resume(); err != nil {
		t.Fatalf("resume gave up entirely: %v", err)
	}
	again.mu.Lock()
	_, good := again.tables["abcdef01"]
	_, bad := again.tables["aaaaaaa1"]
	again.mu.Unlock()
	if !good {
		t.Error("a good table was lost because a bad one could not be read")
	}
	if bad {
		t.Error("a record that could not be rebuilt went live anyway")
	}
}

// A match id is built into a filename, and it arrives from an invitation.
func TestTheFileStoreRefusesAMatchThatIsNotAName(t *testing.T) {
	store, err := NewFileTableStore(filepath.Join(t.TempDir(), "tables"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, bad := range []string{"", "../escape", "a/b", "with space", strings.Repeat("x", 65)} {
		if err := store.SaveTable(TableRecord{Match: bad}); err == nil {
			t.Errorf("%q was accepted as a filename", bad)
		}
	}
}

// The file store is what a real game uses, so it has to round trip.
func TestTheFileStoreRoundTripsATable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tables")
	store, err := NewFileTableStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	rec := TableRecord{
		Match: "abcdef01", GCID: testGCID, Beacon: "aabb",
		Funded: map[uint32]Funded{1: {Outpoint: bondOutpoint, Atoms: 7}},
	}
	rec.Terms.Game, rec.Terms.GameVer, rec.Terms.Seats = "battleships", 1, 2
	rec.Terms.CSVBlocks, rec.Terms.Until, rec.Terms.BondLockBlocks = 2048, 900, 4032
	rec.Terms.SID, rec.Terms.BuyInAtoms, rec.Terms.BondAtoms = "abcdef01", 5_000_000, 2_000_000
	rec.Terms.AccuseFeeAtoms = 10_000
	if err := store.SaveTable(rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	back, err := store.LoadTables()
	if err != nil || len(back) != 1 {
		t.Fatalf("load: %v %d", err, len(back))
	}
	gotHash, err := back[0].Terms.Hash()
	if err != nil {
		t.Fatalf("terms hash: %v", err)
	}
	wantHash, err := rec.Terms.Hash()
	if err != nil {
		t.Fatalf("terms hash: %v", err)
	}
	if gotHash != wantHash {
		t.Error("the terms did not survive the file")
	}
	if back[0].Funded[1].Atoms != 7 || back[0].Funded[1].Outpoint != bondOutpoint {
		t.Errorf("the stake did not survive the file: %+v", back[0].Funded)
	}
	// No half-written file is left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
	if err := store.DropTable("abcdef01"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if back, _ := store.LoadTables(); len(back) != 0 {
		t.Errorf("a dropped table came back: %d", len(back))
	}
}

// keepingGame keeps its own state beside the runtime's, through the optional
// Persisting hook.
type keepingGame struct {
	battleshipsRules
	turn map[string]string
}

func (k *keepingGame) SaveTable(match string) (json.RawMessage, error) {
	return json.Marshal(map[string]string{"turn": k.turn[match]})
}

func (k *keepingGame) LoadTable(match string, blob json.RawMessage) error {
	var got map[string]string
	if err := json.Unmarshal(blob, &got); err != nil {
		return err
	}
	if k.turn == nil {
		k.turn = map[string]string{}
	}
	k.turn[match] = got["turn"]
	return nil
}

// A game's own state travels in the same record and is written at the same
// moment, so the two cannot come back out of step with each other.
func TestAGameGetsItsOwnStateBackWithTheTable(t *testing.T) {
	dir, store := t.TempDir(), NewMemTableStore()
	first := &keepingGame{turn: map[string]string{}}
	_, rt := standAt(t, first, dir, store)
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	first.turn[sid] = "seat 1 to place"
	rt.mu.Lock()
	tbl := rt.tables[sid]
	rt.mu.Unlock()
	rt.keep(tbl)

	second := &keepingGame{}
	_, again := standAt(t, second, dir, store)
	if err := again.Resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := second.turn[sid]; got != "seat 1 to place" {
		t.Fatalf("the game came back with %q", got)
	}
}

// A game that keeps nothing is not an error, and is the common case: most of
// what a game needs is already in the record.
func TestAGameThatKeepsNothingStillResumes(t *testing.T) {
	dir, store := t.TempDir(), NewMemTableStore()
	_, rt := standAt(t, &trivialGame{}, dir, store)
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	_, again := standAt(t, &trivialGame{}, dir, store)
	if err := again.Resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	again.mu.Lock()
	_, ok := again.tables[sid]
	again.mu.Unlock()
	if !ok {
		t.Fatal("a game with no state of its own lost its table")
	}
}

// A runtime with no store keeps nothing and complains about nothing. It is a
// choice a game is allowed to make.
func TestARuntimeWithNoStoreResumesToNothing(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	if err := rt.Resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
}

// Every seat's commit comes back, not only this one's.
//
// This seat's is reproducible by binding again. The others arrived as messages
// nobody will send twice, so a table that lost them comes back a signature
// short and waits for one that is never coming.
func TestACommittedTableComesBackWithEverySeatsCommit(t *testing.T) {
	dir, store := t.TempDir(), NewMemTableStore()
	fake, rt := standAt(t, &trivialGame{}, dir, store)
	sid, them := seatTwo(t, fake, rt)

	rt.mu.Lock()
	tbl := rt.tables[sid]
	rt.mu.Unlock()

	ours, err := tbl.form.Bind()
	if err != nil {
		t.Fatalf("our commit: %v", err)
	}
	theirs, err := them.form.Bind()
	if err != nil {
		t.Fatalf("their commit: %v", err)
	}
	if err := rt.addCommit(sid, theirs); err != nil {
		t.Fatalf("taking their commit: %v", err)
	}
	if err := them.form.AddCommit(ours); err != nil {
		t.Fatalf("them taking ours: %v", err)
	}
	if got := len(tbl.form.Commits()); got != 2 {
		t.Fatalf("the table holds %d commits, so this proves nothing", got)
	}
	wantState := tbl.form.State()

	_, again := standAt(t, &trivialGame{}, dir, store)
	if err := again.Resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	again.mu.Lock()
	after, ok := again.tables[sid]
	again.mu.Unlock()
	if !ok {
		t.Fatal("the table did not come back")
	}
	if got := len(after.form.Commits()); got != 2 {
		t.Fatalf("the table came back with %d of 2 commits; it is a signature short", got)
	}
	if after.form.State() != wantState {
		t.Fatalf("the table came back %v, having been %v", after.form.State(), wantState)
	}
}
