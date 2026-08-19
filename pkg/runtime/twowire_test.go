package runtime

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/decred/dcrd/chaincfg/v3"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/connect"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/wire"
	"github.com/karamble/dcrgaming-sdk/pkg/identity"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

// Two runtimes on one bridge, seating each other with nothing driven by hand.
//
// Every other test here drives the exchange itself - adds the other side's
// join, makes both assertions, sets the beacon. That proves the pieces work
// and hides whether they are wired to each other, which is exactly the fault
// this found: the runtime published a join, waited for agreement, and never
// sent the assertion agreement is made of. No table could ever seat.
func TestTwoRuntimesSeatEachOtherOverTheWire(t *testing.T) {
	fake := bridgetest.New(bridgetest.Options{
		Game: "battleships", Network: "mainnet",
		// Below the invitation's admission deadline, so the table is not
		// already late before anybody has joined it.
		Params: chaincfg.TestNet3Params(), Height: 800,
	})
	srv, err := fake.Serve("seat0", "seat1")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	one := wireSeat(t, ctx, srv, "seat0")
	two := wireSeat(t, ctx, srv, "seat1")
	go func() { _ = one.Run(ctx) }()
	go func() { _ = two.Run(ctx) }()
	// Both are subscribed before anybody speaks, or the first join is sent
	// to nobody.
	waitFor(t, "both seats subscribed", func() bool { return fake.Subscribers() == 2 })

	link := invite(t, nil)
	sid, err := accept(one, link, testGCID)
	if err != nil {
		t.Fatalf("first seat accepting: %v", err)
	}
	if _, err := accept(two, link, testGCID); err != nil {
		t.Fatalf("second seat accepting: %v", err)
	}

	// The beacon is drawn from a block ahead of the deadline, so the chain
	// has to reach it. Ticking is what a game does on every new block.
	defer func() {
		for name, rt := range map[string]*Runtime{"one": one, "two": two} {
			rt.mu.Lock()
			tbl := rt.tables[sid]
			if tbl != nil {
				seats, seated := tbl.form.Seats()
				t.Logf("%s: state=%v joins=%d commits=%d agreed=%v seats=%d/%v closed=%v",
					name, tbl.form.State(), len(tbl.form.Joins()), len(tbl.form.Commits()), tbl.form.Agreed(),
					len(seats), seated, tbl.form.WindowClosed())
			}
			rt.mu.Unlock()
		}
		t.Logf("frames on the wire: sent=%d relayed=%d", len(fake.Sent()), fake.Relayed())
	}()
	// A block at a time, which is the cadence a game ticks at. One of the
	// first joins is dropped as unauthorized - it reaches a peer that has
	// not accepted the invitation yet - and the repeat is what recovers it.
	block := func() {
		fake.Mine(1)
		one.Tick(ctx, fake.Height())
		two.Tick(ctx, fake.Height())
	}
	waitFor(t, "both sides agreed", func() bool {
		block()
		return agreed(one, sid) && agreed(two, sid)
	})
	waitFor(t, "both sides seated", func() bool {
		block()
		_, a := one.Seats(sid)
		_, b := two.Seats(sid)
		return a && b
	})

	// Settled, which is every member bound to this membership - and with
	// admission still open, so it happened on unanimity rather than by
	// sitting out the deadline. A table that could only form the slow way
	// is a lobby nobody watches.
	for name, rt := range map[string]*Runtime{"one": one, "two": two} {
		tbl, err := rt.tableOf(sid)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if tbl.form.State() != membership.Settled {
			t.Fatalf("%s reached %v, not settled", name, tbl.form.State())
		}
		if got := len(tbl.form.Commits()); got != 2 {
			t.Fatalf("%s holds %d of 2 commits", name, got)
		}
	}

	// And they seated the same table.
	first, _ := one.Seats(sid)
	second, _ := two.Seats(sid)
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("the tables hold %d and %d seats", len(first), len(second))
	}
	for seat, key := range first {
		if bytesEqual(second[seat], key) {
			continue
		}
		t.Fatalf("the two sides disagree about who is in seat %d", seat)
	}
	oneAt, ok := one.tables[sid].form.OurSeat()
	if !ok {
		t.Fatal("the first seat is not seated at its own table")
	}
	twoAt, ok := two.tables[sid].form.OurSeat()
	if !ok {
		t.Fatal("the second seat is not seated at its own table")
	}
	if oneAt == twoAt {
		t.Fatalf("both peers took seat %d", oneAt)
	}
}

func agreed(rt *Runtime, sid string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	t, ok := rt.tables[sid]
	return ok && t.form.Agreed()
}

// waitFor polls until want is true, or gives up and says what it was waiting
// for.
func waitFor(t *testing.T, what string, want func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if want() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// wireSeat is one runtime with its own identity, dialled as its own seat.
func wireSeat(t *testing.T, ctx context.Context, srv *bridgetest.Server, seat string) *Runtime {
	t.Helper()
	g := &trivialGame{}
	id := g.Identity()
	conn, err := srv.Dial(ctx, seat, func(cfg *transport.BridgeConfig) { connect.Stamp(cfg, id) })
	if err != nil {
		t.Fatalf("dial as %s: %v", seat, err)
	}
	book, err := spend.OpenBook(spend.MemStore())
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	seed, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	// A join binds to its bond, so a seat needs one before it can join. Its
	// own, or both peers would join with the same key.
	if err := seed.SetBondDeposit(bondFor(seat)); err != nil {
		t.Fatalf("bond deposit: %v", err)
	}
	rt, err := New(Config{
		Rules: g, Bridge: conn, Book: book,
		Identity: seed, SeatTags: testTags, Params: chaincfg.TestNet3Params(),
		PunishTag: []byte("testgame/punishkey/v1"),
		Tables:    NewMemTableStore(),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return rt
}

func bondFor(seat string) string {
	if seat == "seat0" {
		return bondOutpoint
	}
	return "bb22cc33bb22cc33bb22cc33bb22cc33bb22cc33bb22cc33bb22cc33bb22cc33:1"
}

// A table agrees without waiting for a block.
//
// Answering an arriving join with what this peer now holds is what does it. The
// per-block repeat would get there too, so this is not what makes seating
// work - it is what makes it not cost a block, and a lobby that waits for one
// is a lobby people leave.
func TestATableAgreesWithoutWaitingForABlock(t *testing.T) {
	fake := bridgetest.New(bridgetest.Options{
		Game: "battleships", Network: "mainnet",
		Params: chaincfg.TestNet3Params(), Height: 800,
	})
	srv, err := fake.Serve("seat0", "seat1")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	one := wireSeat(t, ctx, srv, "seat0")
	two := wireSeat(t, ctx, srv, "seat1")
	go func() { _ = one.Run(ctx) }()
	go func() { _ = two.Run(ctx) }()
	waitFor(t, "both seats subscribed", func() bool { return fake.Subscribers() == 2 })

	link := invite(t, nil)
	// Both accept before either speaks, so no join is dropped and the
	// exchange has to finish on its own.
	sid, err := accept(one, link, testGCID)
	if err != nil {
		t.Fatalf("first seat accepting: %v", err)
	}
	if _, err := accept(two, link, testGCID); err != nil {
		t.Fatalf("second seat accepting: %v", err)
	}

	// Not one tick. If agreement needed a block this would never come.
	waitFor(t, "both sides agreed without a block", func() bool {
		return agreed(one, sid) && agreed(two, sid)
	})
	if fake.Height() != 800 {
		t.Fatalf("the chain moved to %d, so a block may have done this", fake.Height())
	}

	// And they stop. A roster is answered only when it says something new,
	// so two peers that already agree fall silent instead of answering each
	// other forever.
	settled := len(fake.Sent())
	time.Sleep(200 * time.Millisecond)
	if grown := len(fake.Sent()) - settled; grown > 0 {
		t.Fatalf("%d more frames went out after both sides agreed; they are answering each other", grown)
	}
}

// A table whose opening messages were all lost still forms.
//
// The join and the roster are each sent once when something happens. If the
// bridge is down at that moment, nothing is ever sent again by that path, and
// two peers sit holding one join apiece until the deadline takes the table
// away with the buy-in already committed. The block repeat is what recovers
// it, and it is the only thing that does.
func TestATableWhoseOpeningMessagesWereLostStillForms(t *testing.T) {
	fake := bridgetest.New(bridgetest.Options{
		Game: "battleships", Network: "mainnet",
		Params: chaincfg.TestNet3Params(), Height: 800,
	})
	srv, err := fake.Serve("seat0", "seat1")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	one := wireSeat(t, ctx, srv, "seat0")
	two := wireSeat(t, ctx, srv, "seat1")
	go func() { _ = one.Run(ctx) }()
	go func() { _ = two.Run(ctx) }()
	waitFor(t, "both seats subscribed", func() bool { return fake.Subscribers() == 2 })

	// Nothing gets through while they join.
	fake.SetUnreachable(true)
	link := invite(t, nil)
	sid := "abcdef01"
	if _, err := accept(one, link, testGCID); err == nil {
		t.Fatal("a join was sent while the bridge was down")
	}
	if _, err := accept(two, link, testGCID); err == nil {
		t.Fatal("a join was sent while the bridge was down")
	}
	// Both hold a table and neither has told anybody about it.
	for name, rt := range map[string]*Runtime{"one": one, "two": two} {
		if _, err := rt.tableOf(sid); err != nil {
			t.Fatalf("%s has no table to recover: %v", name, err)
		}
	}

	fake.SetUnreachable(false)
	waitFor(t, "the table formed from the block repeat alone", func() bool {
		fake.Mine(1)
		one.Tick(ctx, fake.Height())
		two.Tick(ctx, fake.Height())
		return agreed(one, sid) && agreed(two, sid)
	})
}

// A peer that missed a commit gets it back by asking.
//
// Formation messages go out when something happens. A peer whose stream was
// down while somebody committed is short a signature its table needs to
// settle, and nothing on the sending side would ever send it again. Saying
// what we hold does not fix it either - the gap is in what we never heard - so
// it asks, naming what it has, and the table answers with the difference.
func TestAPeerThatMissedACommitAsksForIt(t *testing.T) {
	fake := bridgetest.New(bridgetest.Options{
		Game: "battleships", Network: "mainnet",
		Params: chaincfg.TestNet3Params(), Height: 800,
	})
	srv, err := fake.Serve("seat0", "seat1")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	one := wireSeat(t, ctx, srv, "seat0")
	two := wireSeat(t, ctx, srv, "seat1")
	go func() { _ = one.Run(ctx) }()
	go func() { _ = two.Run(ctx) }()
	waitFor(t, "both seats subscribed", func() bool { return fake.Subscribers() == 2 })

	link := invite(t, nil)
	sid, err := accept(one, link, testGCID)
	if err != nil {
		t.Fatalf("first seat accepting: %v", err)
	}
	if _, err := accept(two, link, testGCID); err != nil {
		t.Fatalf("second seat accepting: %v", err)
	}
	waitFor(t, "both settled", func() bool {
		fake.Mine(1)
		one.Tick(ctx, fake.Height())
		two.Tick(ctx, fake.Height())
		return commits(one, sid) == 2 && commits(two, sid) == 2
	})

	// Take one seat's copy of the other's commit away, the way a stream
	// that was down would have left it: everything else intact.
	tbl, err := one.tableOf(sid)
	if err != nil {
		t.Fatalf("table: %v", err)
	}
	theirs := forgetTheirCommit(t, one, tbl)
	if commits(one, sid) != 1 {
		t.Fatalf("the commit was not taken away: %d remain", commits(one, sid))
	}

	// Asked for directly, with no block in between: the per-block repair
	// would fetch it too, and this is about the gap the bridge reports.
	one.Resync(ctx)
	waitFor(t, "the missing commit to come back", func() bool { return commits(one, sid) == 2 })

	// The same commit, not some other one.
	back, err := one.tableOf(sid)
	if err != nil {
		t.Fatalf("table: %v", err)
	}
	var found bool
	for _, c := range back.form.Commits() {
		if bytesEqual(c.Signer, theirs) {
			found = true
		}
	}
	if !found {
		t.Fatal("a commit came back, but not the one that went missing")
	}
}

func commits(rt *Runtime, sid string) int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	t, ok := rt.tables[sid]
	if !ok {
		return 0
	}
	return len(t.form.Commits())
}

// forgetTheirCommit rebuilds a table's formation without the other seat's
// commit, standing in for a stream that was down while it was published. It
// returns the signer that was dropped.
func forgetTheirCommit(t *testing.T, rt *Runtime, tbl *table) []byte {
	t.Helper()
	// Identified by its signer rather than its seat: seating needs the
	// beacon, and this table has agreed without waiting for that block.
	session, _, err := rt.seatKeys(tbl.form.Terms().SID)
	if err != nil {
		t.Fatalf("seat keys: %v", err)
	}
	ours := session.PubKey().SerializeCompressed()
	var dropped []byte
	for _, c := range tbl.form.Commits() {
		if !bytesEqual(c.Signer, ours) {
			dropped = c.Signer
		}
	}
	if len(dropped) == 0 {
		t.Fatal("this table holds no commit but its own, so there is nothing to lose")
	}

	creds, err := rt.seatCredentials(tbl, tbl.form.Terms())
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	form, err := membership.NewFormation(tbl.form.Terms(), creds)
	if err != nil {
		t.Fatalf("formation: %v", err)
	}
	for _, j := range tbl.form.Joins() {
		if err := form.AddJoin(j); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	if _, err := form.Bind(); err != nil {
		t.Fatalf("bind: %v", err)
	}
	rt.mu.Lock()
	tbl.form = form
	rt.mu.Unlock()
	return dropped
}

// A resync answer carries the difference and nothing else.
//
// Correctness does not depend on this - a peer that was sent the whole table
// would adopt what it already had and be no worse off. Bandwidth does: a
// resync happens on every reconnect, and an answer that repeated the whole
// membership each time would be the largest message this protocol sends and
// the one most often sent.
func TestAResyncAnswerCarriesOnlyTheDifference(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	ctx := context.Background()
	tbl, err := rt.tableOf(sid)
	if err != nil {
		t.Fatalf("table: %v", err)
	}
	// Two commits at this table, so there is something to leave out.
	ours, err := tbl.form.Bind()
	if err != nil {
		t.Fatalf("our commit: %v", err)
	}
	theirs, err := them.form.Bind()
	if err != nil {
		t.Fatalf("their commit: %v", err)
	}
	if err := rt.addCommit(ctx, sid, theirs); err != nil {
		t.Fatalf("their commit: %v", err)
	}

	// What this peer would actually ask, not a hand-built one: naming
	// everything held is what keeps the answer small.
	full := resyncAsk(tbl)
	if len(full.Joins) != 2 || len(full.Commits) != 2 {
		t.Fatalf("this table holds %d joins and %d commits, so this proves nothing",
			len(full.Joins), len(full.Commits))
	}

	// An asker that holds everything is told nothing.
	if got := resyncDiff(tbl, full); len(got.Joins) != 0 || len(got.Commits) != 0 {
		t.Errorf("a peer that was already in step was sent %d joins and %d commits",
			len(got.Joins), len(got.Commits))
	}
	// And nothing goes on the wire for it either.
	before := len(fake.Sent())
	if err := rt.answerResync(ctx, sid, full); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if grown := len(fake.Sent()) - before; grown != 0 {
		t.Errorf("%d frames went out answering a peer with nothing to learn", grown)
	}

	// An asker short of one commit is told that one, not both.
	short := schema.Resync{Joins: full.Joins}
	for _, c := range tbl.form.Commits() {
		if !bytesEqual(c.Signer, theirs.Signer) {
			short.Commits = append(short.Commits, hex.EncodeToString(c.Signer))
		}
	}
	got := resyncDiff(tbl, short)
	if len(got.Joins) != 0 {
		t.Errorf("%d joins were sent to a peer that named them all", len(got.Joins))
	}
	if len(got.Commits) != 1 {
		t.Fatalf("a peer short of one commit was sent %d", len(got.Commits))
	}
	back, err := got.Commits[0].Into()
	if err != nil {
		t.Fatalf("the answer does not read back: %v", err)
	}
	if !bytesEqual(back.Signer, theirs.Signer) {
		t.Error("the wrong commit was sent")
	}
	_ = ours
}

// A peer that missed a commit gets it back without anybody noticing a gap.
//
// The bridge reports the gaps it knows about, and that is not all of them: a
// frame dropped for a table a peer had not joined yet, or lost anywhere the
// bridge is not looking, is a message nobody will resend and nobody will ask
// for. A table that is still forming therefore asks every block, rather than
// waiting to be told it missed something.
func TestATableStillFormingAsksEveryBlock(t *testing.T) {
	fake := bridgetest.New(bridgetest.Options{
		Game: "battleships", Network: "mainnet",
		Params: chaincfg.TestNet3Params(), Height: 800,
	})
	srv, err := fake.Serve("seat0", "seat1")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	one := wireSeat(t, ctx, srv, "seat0")
	two := wireSeat(t, ctx, srv, "seat1")
	go func() { _ = one.Run(ctx) }()
	go func() { _ = two.Run(ctx) }()
	waitFor(t, "both seats subscribed", func() bool { return fake.Subscribers() == 2 })

	link := invite(t, nil)
	sid, err := accept(one, link, testGCID)
	if err != nil {
		t.Fatalf("first seat accepting: %v", err)
	}
	if _, err := accept(two, link, testGCID); err != nil {
		t.Fatalf("second seat accepting: %v", err)
	}
	waitFor(t, "both settled", func() bool {
		fake.Mine(1)
		one.Tick(ctx, fake.Height())
		two.Tick(ctx, fake.Height())
		return commits(one, sid) == 2 && commits(two, sid) == 2
	})

	// Lose it, and tell nobody. No gap is reported and no Resync is called.
	tbl, err := one.tableOf(sid)
	if err != nil {
		t.Fatalf("table: %v", err)
	}
	forgetTheirCommit(t, one, tbl)
	if commits(one, sid) != 1 {
		t.Fatalf("the commit was not taken away: %d remain", commits(one, sid))
	}

	waitFor(t, "the block asking to bring it back", func() bool {
		fake.Mine(1)
		one.Tick(ctx, fake.Height())
		two.Tick(ctx, fake.Height())
		return commits(one, sid) == 2
	})
}

// Two peers build each other's accusation chains, over the wire.
//
// This is the forfeiture path's foundation and the part no single-peer test
// can reach: a chain is built over the opponent's forfeitable bond, and every
// rung needs the opponent's signature - including the accused's, gathered
// while everybody is still cooperating, because it cannot be gathered when it
// is needed.
//
// It also needs each side to know where the other's bond is, which is not on
// the chain in any findable way: only the payer saw it land, so it is
// announced. A runtime that could not read that announcement would sit with a
// chain it could never build and a cheat it could never punish.
func TestTwoRuntimesBuildEachOthersLadders(t *testing.T) {
	fake := bridgetest.New(bridgetest.Options{
		Game: "battleships", Network: "mainnet",
		Params: chaincfg.TestNet3Params(), Height: 800,
	})
	srv, err := fake.Serve("seat0", "seat1")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	one := wireSeat(t, ctx, srv, "seat0")
	two := wireSeat(t, ctx, srv, "seat1")
	go func() { _ = one.Run(ctx) }()
	go func() { _ = two.Run(ctx) }()
	waitFor(t, "both seats subscribed", func() bool { return fake.Subscribers() == 2 })

	link := invite(t, nil)
	sid, err := accept(one, link, testGCID)
	if err != nil {
		t.Fatalf("first seat accepting: %v", err)
	}
	if _, err := accept(two, link, testGCID); err != nil {
		t.Fatalf("second seat accepting: %v", err)
	}
	block := func() {
		fake.Mine(1)
		one.Tick(ctx, fake.Height())
		two.Tick(ctx, fake.Height())
	}
	waitFor(t, "both sides seated", func() bool {
		block()
		_, a := one.Seats(sid)
		_, b := two.Seats(sid)
		return a && b
	})

	// Both forfeitable bonds exist only once both seats have announced the
	// key that can take them. The announcement goes out at seating, when
	// the other side may not be seated yet to hear it, so what gets it
	// there is the repeat on the next block.
	waitFor(t, "both sides to derive both forfeitable bonds", func() bool {
		block()
		for _, rt := range []*Runtime{one, two} {
			for _, seat := range []uint32{0, 1} {
				if _, ok := rt.ForfeitableBond(sid, seat); !ok {
					return false
				}
			}
		}
		return true
	})

	// Both bonds, because they answer different faults: the chain built
	// below spends the table bond, which is what a seat stakes against
	// going quiet, and the forfeitable bond is taken whole for a proven
	// lie. Each seat pays its own and tells the other where they are.
	for name, rt := range map[string]*Runtime{"one": one, "two": two} {
		if err := rt.FundTableBond(ctx, sid); err != nil {
			t.Fatalf("%s's table bond: %v", name, err)
		}
		if err := rt.FundForfeitBond(ctx, sid); err != nil {
			t.Fatalf("%s's forfeitable bond: %v", name, err)
		}
	}
	waitFor(t, "each side to learn where the other's bonds are", func() bool {
		block()
		return bothBondsKnown(one, sid) && bothBondsKnown(two, sid)
	})

	// Now each can build the chain that punishes the other, and each needs
	// the other's signature on it.
	if err := one.PresignLadder(ctx, sid); err != nil {
		t.Fatalf("first seat presigning: %v", err)
	}
	if err := two.PresignLadder(ctx, sid); err != nil {
		t.Fatalf("second seat presigning: %v", err)
	}
	// Every rung, not merely some: a chain is run one rung at a time and a
	// rung short of a signature is a rung the attrition stops at.
	want := rungsAgainstOpponent(t, one, sid)
	if want == 0 {
		t.Fatal("the chain has no rungs, so this proves nothing")
	}
	waitFor(t, "every rung of both chains to be co-signed", func() bool {
		block()
		a, _, aok := one.Ladder(sid)
		b, _, bok := two.Ladder(sid)
		return aok && bok && a == want && b == want
	})

	a, aRun, _ := one.Ladder(sid)
	b, bRun, _ := two.Ladder(sid)
	if a != b || a != want {
		t.Fatalf("the two sides hold %d and %d co-signed rungs of %d", a, b, want)
	}
	if aRun != 0 || bRun != 0 {
		t.Fatalf("a rung has been run before anybody was accused: %d and %d", aRun, bRun)
	}

	// And the asymmetric case, which is the one that hangs: a peer that is
	// short of a signature while the other has everything. The one with
	// everything has nothing of its own left to say, so unless a repeat is
	// answered it never speaks again and the short one waits forever.
	forgetTheirRungSignature(t, two, sid)
	if got, _, _ := two.Ladder(sid); got == want {
		t.Fatal("the signature was not taken away, so this proves nothing")
	}
	if got, _, _ := one.Ladder(sid); got != want {
		t.Fatalf("the other side is short too (%d of %d), so this tests the wrong thing", got, want)
	}
	waitFor(t, "the missing rung signature to come back", func() bool {
		block()
		got, _, _ := two.Ladder(sid)
		return got == want
	})
}

// forgetTheirRungSignature drops one seat's copy of the opponent's signature on
// the first rung, the way a message that never arrived would have left it.
func forgetTheirRungSignature(t *testing.T, rt *Runtime, sid string) {
	t.Helper()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	tbl := rt.tables[sid]
	mine, _ := tbl.form.OurSeat()
	seats, _ := tbl.form.Seats()
	l := tbl.ladders[1-mine]
	if l == nil || len(l.rungs) == 0 {
		t.Fatal("no chain to forget anything from")
	}
	mineHex := hex.EncodeToString(seats[mine])
	for signer := range l.sigs[0] {
		if signer != mineHex {
			delete(l.sigs[0], signer)
		}
	}
	l.ready[0] = nil
}

// rungsAgainstOpponent is how many rungs the chain this peer could run has.
func rungsAgainstOpponent(t *testing.T, rt *Runtime, sid string) int {
	t.Helper()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	tbl, ok := rt.tables[sid]
	if !ok {
		return 0
	}
	mine, seated := tbl.form.OurSeat()
	if !seated {
		return 0
	}
	l := tbl.ladders[1-mine]
	if l == nil {
		return 0
	}
	return len(l.rungs)
}

// bothBondsKnown reports whether a peer knows where every seat's forfeitable
// bond is.
func bothBondsKnown(rt *Runtime, sid string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	t, ok := rt.tables[sid]
	if !ok {
		return false
	}
	for _, seat := range []uint32{0, 1} {
		if t.forfeitFunded[seat].outpoint == "" || t.tableBondFunded[seat].outpoint == "" {
			return false
		}
	}
	return true
}

// Two peers pay a table out, over the wire.
//
// The whole point of the thing. Everything else is arrangement; this is the
// money moving, and it needs every seat's signature on one transaction: each
// stake sits behind its own script and the settlement branch of every one of
// them names the whole table, so a settlement one signature short is a table
// that falls back to its refund timelocks - weeks of everybody's money locked
// up because two peers could not finish a conversation.
func TestTwoRuntimesPayATableOut(t *testing.T) {
	fake := bridgetest.New(bridgetest.Options{
		Game: "battleships", Network: "mainnet",
		Params: chaincfg.TestNet3Params(), Height: 800,
	})
	srv, err := fake.Serve("seat0", "seat1")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	one := wireSeat(t, ctx, srv, "seat0")
	two := wireSeat(t, ctx, srv, "seat1")
	go func() { _ = one.Run(ctx) }()
	go func() { _ = two.Run(ctx) }()
	waitFor(t, "both seats subscribed", func() bool { return fake.Subscribers() == 2 })

	link := invite(t, nil)
	sid, err := accept(one, link, testGCID)
	if err != nil {
		t.Fatalf("first seat accepting: %v", err)
	}
	if _, err := accept(two, link, testGCID); err != nil {
		t.Fatalf("second seat accepting: %v", err)
	}
	block := func() {
		fake.Mine(1)
		one.Tick(ctx, fake.Height())
		two.Tick(ctx, fake.Height())
	}
	waitFor(t, "both sides seated", func() bool {
		block()
		_, a := one.Seats(sid)
		_, b := two.Seats(sid)
		return a && b
	})

	// Where each wants to be paid, and the stake each is playing for.
	for name, rt := range map[string]*Runtime{"one": one, "two": two} {
		if err := rt.setPayout(ctx, payTo(t)); err != nil {
			t.Fatalf("%s's payout: %v", name, err)
		}
		if err := rt.Fund(ctx, sid); err != nil {
			t.Fatalf("%s funding its stake: %v", name, err)
		}
	}
	waitFor(t, "both stakes and both payouts to be known on both sides", func() bool {
		block()
		return tableReadyToSettle(one, sid) && tableReadyToSettle(two, sid)
	})

	// The game says who won; the runtime has no opinion about it. Both
	// sides say the same thing, because they are running the same rules.
	mine, _ := seatOfRuntime(one, sid)
	// The shares divide what the table holds; the fee comes out of the
	// transaction, not out of the arithmetic the seats agree.
	pot := int64(one.Terms(sid).BuyInAtoms) * 2
	out := Outcome{Shares: map[uint32]int64{mine: pot}}
	if err := one.Settle(ctx, sid, out); err != nil {
		t.Fatalf("first seat settling: %v", err)
	}
	if err := two.Settle(ctx, sid, out); err != nil {
		t.Fatalf("second seat settling: %v", err)
	}

	waitFor(t, "both sides to know the table has been paid out", func() bool {
		block()
		return settlementDone(one, sid) && settlementDone(two, sid)
	})
	// One transaction, whoever sent it. Both peers holding every signature
	// will both send it, and that is wanted rather than tolerated: it is
	// the same bytes, so a chain takes the first and tells the second it
	// already has it, and neither peer depends on the other actually
	// sending the thing that pays them. What would be a double spend is two
	// *different* transactions, which is what this counts.
	sent := fake.Broadcasts()
	if len(sent) != 1 {
		t.Fatalf("%d different transactions were broadcast, want one settlement: %v", len(sent), sent)
	}
	// And both sides agree it is done, or one of them is still waiting to
	// be paid.
	for name, rt := range map[string]*Runtime{"one": one, "two": two} {
		if !settlementDone(rt, sid) {
			t.Fatalf("%s does not know its table has been paid out", name)
		}
	}

	// The asymmetric case: one side short of the other's signature while
	// the other has everything. The one with everything has nothing of its
	// own left to say, so unless a repeat is answered the short one waits
	// for a message that will never come again.
	forgetTheirPayoutSignature(t, two, sid)
	if settlementDone(two, sid) {
		t.Fatal("the signature was not taken away, so this proves nothing")
	}
	if !settlementDone(one, sid) {
		t.Fatal("the other side is short too, so this tests the wrong thing")
	}
	waitFor(t, "the missing payout signature to come back", func() bool {
		block()
		return settlementDone(two, sid)
	})

	// And then it stops. Checked last, after the answering above, because
	// that is where an answer indistinguishable from a request would send
	// the two of them into replying to each other for as long as the table
	// lived - and a check taken before it would never see it.
	for range 4 {
		block()
	}
	quiet := len(fake.Sent())
	for range 4 {
		block()
	}
	if grown := len(fake.Sent()) - quiet; grown > 0 {
		t.Errorf("%d frames went out over four blocks after the table was paid and settled", grown)
	}
}

// forgetTheirPayoutSignature drops one seat's copy of the opponent's signature
// on the payout, the way a message that never arrived would have left it.
func forgetTheirPayoutSignature(t *testing.T, rt *Runtime, sid string) {
	t.Helper()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	tbl := rt.tables[sid]
	if tbl.settle == nil {
		t.Fatal("no payout to forget anything from")
	}
	mine, _ := tbl.form.OurSeat()
	seats, _ := tbl.form.Seats()
	mineHex := hex.EncodeToString(seats[mine])
	for signer := range tbl.settle.sigs {
		if signer != mineHex {
			delete(tbl.settle.sigs, signer)
		}
	}
	tbl.settle.done = false
}

// tableReadyToSettle reports whether a peer knows every stake and every payout.
func tableReadyToSettle(rt *Runtime, sid string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	t, ok := rt.tables[sid]
	if !ok || t.form == nil {
		return false
	}
	seats, seated := t.form.Seats()
	if !seated {
		return false
	}
	return len(t.funded) == len(seats) && len(t.payouts) == len(seats)
}

// seatOfRuntime is this peer's own seat.
func seatOfRuntime(rt *Runtime, sid string) (uint32, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	t, ok := rt.tables[sid]
	if !ok || t.form == nil {
		return 0, false
	}
	return t.form.OurSeat()
}

// settlementDone reports whether a peer believes its table has been paid out.
func settlementDone(rt *Runtime, sid string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	t, ok := rt.tables[sid]
	return ok && t.settle != nil && t.settle.done
}

// Both seats get their table bond back, over the wire.
//
// A release is the cooperative way out: the bond goes home now rather than
// when its lock matures. Each seat has its own - a release pays its owner and
// nobody else - so there are two of them, and each needs the other seat's
// signature. Two peers releasing at the same time is the ordinary case, not an
// edge: a table ends for both of them at once.
func TestBothSeatsGetTheirTableBondBack(t *testing.T) {
	fake := bridgetest.New(bridgetest.Options{
		Game: "battleships", Network: "mainnet",
		Params: chaincfg.TestNet3Params(), Height: 800,
	})
	srv, err := fake.Serve("seat0", "seat1")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	one := wireSeat(t, ctx, srv, "seat0")
	two := wireSeat(t, ctx, srv, "seat1")
	go func() { _ = one.Run(ctx) }()
	go func() { _ = two.Run(ctx) }()
	waitFor(t, "both seats subscribed", func() bool { return fake.Subscribers() == 2 })

	link := invite(t, nil)
	sid, err := accept(one, link, testGCID)
	if err != nil {
		t.Fatalf("first seat accepting: %v", err)
	}
	if _, err := accept(two, link, testGCID); err != nil {
		t.Fatalf("second seat accepting: %v", err)
	}
	block := func() {
		fake.Mine(1)
		one.Tick(ctx, fake.Height())
		two.Tick(ctx, fake.Height())
	}
	waitFor(t, "both sides seated", func() bool {
		block()
		_, a := one.Seats(sid)
		_, b := two.Seats(sid)
		return a && b
	})

	// A release pays its owner, so each seat has to have said where.
	for name, rt := range map[string]*Runtime{"one": one, "two": two} {
		if err := rt.setPayout(ctx, payTo(t)); err != nil {
			t.Fatalf("%s's payout: %v", name, err)
		}
		if err := rt.FundTableBond(ctx, sid); err != nil {
			t.Fatalf("%s's table bond: %v", name, err)
		}
	}
	waitFor(t, "both bonds and both payouts to be known on both sides", func() bool {
		block()
		return bondsAndPayoutsKnown(one, sid) && bondsAndPayoutsKnown(two, sid)
	})

	// Both at once, which is what the end of a table looks like.
	if err := one.releaseTableBond(ctx, sid); err != nil {
		t.Fatalf("first seat releasing: %v", err)
	}
	if err := two.releaseTableBond(ctx, sid); err != nil {
		t.Fatalf("second seat releasing: %v", err)
	}

	// Two releases, one per seat, each spending that seat's own bond.
	waitFor(t, "both bonds to go home", func() bool {
		block()
		return len(fake.Broadcasts()) >= 2
	})
	sent := fake.Broadcasts()
	if len(sent) != 2 {
		t.Fatalf("%d transactions were broadcast, want one release per seat: %v", len(sent), sent)
	}
	for _, rt := range []*Runtime{one, two} {
		if !releaseSent(rt, sid) {
			t.Error("a seat does not know its own bond went home")
		}
	}
}

// bondsAndPayoutsKnown reports whether a peer knows every table bond and every
// payout.
func bondsAndPayoutsKnown(rt *Runtime, sid string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	t, ok := rt.tables[sid]
	if !ok || t.form == nil {
		return false
	}
	seats, seated := t.form.Seats()
	if !seated {
		return false
	}
	return len(t.tableBondFunded) == len(seats) && len(t.payouts) == len(seats)
}

// releaseSent reports whether a peer has sent its own release.
func releaseSent(rt *Runtime, sid string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	t, ok := rt.tables[sid]
	if !ok {
		return false
	}
	mine, seated := t.form.OurSeat()
	if !seated {
		return false
	}
	rel, held := t.releases[mine]
	return held && rel.done
}

// A game's own message crosses between two peers, and the runtime's do not.
//
// Handle is only half a contract. A game that can hear its opponent and not
// answer cannot be played, and until there was a way to send one no game had
// ever been hosted here to notice.
func TestAGamesOwnMessageCrossesAndTheRuntimesCannotBeForged(t *testing.T) {
	fake := bridgetest.New(bridgetest.Options{
		Game: "battleships", Network: "mainnet",
		Params: chaincfg.TestNet3Params(), Height: 800,
	})
	srv, err := fake.Serve("seat0", "seat1")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	heard := make(chan Message, 8)
	one := wireSeat(t, ctx, srv, "seat0")
	two := wireSeatListening(t, ctx, srv, "seat1", heard)
	go func() { _ = one.Run(ctx) }()
	go func() { _ = two.Run(ctx) }()
	waitFor(t, "both seats subscribed", func() bool { return fake.Subscribers() == 2 })

	link := invite(t, nil)
	sid, err := accept(one, link, testGCID)
	if err != nil {
		t.Fatalf("first seat accepting: %v", err)
	}
	if _, err := accept(two, link, testGCID); err != nil {
		t.Fatalf("second seat accepting: %v", err)
	}
	waitFor(t, "both sides seated", func() bool {
		fake.Mine(1)
		one.Tick(ctx, fake.Height())
		two.Tick(ctx, fake.Height())
		_, a := one.Seats(sid)
		_, b := two.Seats(sid)
		return a && b
	})

	if err := one.Send(ctx, sid, "shoot", map[string]any{"x": 3, "y": 4}, wire.ClassTurn); err != nil {
		t.Fatalf("send a shot: %v", err)
	}
	select {
	case got := <-heard:
		if got.Kind != "shoot" {
			t.Fatalf("the other side heard a %q", got.Kind)
		}
		if got.Match != sid {
			t.Fatalf("it arrived for table %q", got.Match)
		}
		var body struct{ X, Y int }
		if err := json.Unmarshal(got.Body, &body); err != nil {
			t.Fatalf("the body did not survive: %v", err)
		}
		if body.X != 3 || body.Y != 4 {
			t.Fatalf("the shot arrived as %d,%d", body.X, body.Y)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the other side never heard it")
	}

	// A message with no kind is not a message; the router would frame an
	// envelope nothing can dispatch.
	if err := one.Send(ctx, sid, "   ", map[string]any{}, wire.ClassTurn); err == nil {
		t.Error("a message with no kind was sent")
	}

	// And the runtime's own are not the game's to send. A game that could
	// forge a settlement could arrange the money differently.
	for _, kind := range []schema.Kind{
		schema.KindJoin, schema.KindCommit, schema.KindSettle,
		KindRoster, KindFunded, KindBonded, KindPayout, KindRelease,
		KindAccusation, KindPunishKey, KindResync, KindResyncReply,
	} {
		err := one.Send(ctx, sid, kind, map[string]any{}, wire.ClassTurn)
		if err == nil {
			t.Errorf("a game was allowed to send %q", kind)
			continue
		}
		if !strings.Contains(err.Error(), "not a game's to send") {
			t.Errorf("%s refused for the wrong reason: %v", kind, err)
		}
	}
}

// listeningRules is a game that keeps what it hears.
type listeningRules struct {
	*battleshipsRules
	heard chan Message
}

func (l *listeningRules) Handle(_ context.Context, in Message) error {
	select {
	case l.heard <- in:
	default:
	}
	return nil
}

// wireSeatListening is wireSeat with a game that reports what reaches it.
func wireSeatListening(t *testing.T, ctx context.Context, srv *bridgetest.Server,
	seat string, heard chan Message) *Runtime {

	t.Helper()
	g := &listeningRules{battleshipsRules: &battleshipsRules{}, heard: heard}
	conn, err := srv.Dial(ctx, seat, func(cfg *transport.BridgeConfig) { connect.Stamp(cfg, g.Identity()) })
	if err != nil {
		t.Fatalf("dial as %s: %v", seat, err)
	}
	book, err := spend.OpenBook(spend.MemStore())
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	seed, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if err := seed.SetBondDeposit(bondFor(seat)); err != nil {
		t.Fatalf("bond deposit: %v", err)
	}
	rt, err := New(Config{
		Rules: g, Bridge: conn, Book: book, Tables: NewMemTableStore(),
		Identity: seed, SeatTags: testTags, Params: chaincfg.TestNet3Params(),
		PunishTag: []byte("testgame/punishkey/v1"),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return rt
}

// The runtime satisfies the facade it publishes.
//
// The interface is what a game's own tests stand in for, so a method declared
// there and not implemented here is a game that compiles against a runtime it
// cannot actually be given - which is how Send came to be declared for a long
// time and never written.
var _ Game = (*Runtime)(nil)

// A game is handed its log key and nothing else it could spend with.
//
// The split is the point: the log key signs what a game says happened and is
// expected to become public the moment its owner equivocates, which is what
// makes a lie provable. The session key holds the stake, and a game holding it
// could move its own money.
func TestAGameIsHandedItsLogKeyAndNotItsSessionKey(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := seatTwo(t, fake, rt)

	lk, err := rt.LogKey(sid)
	if err != nil {
		t.Fatalf("log key: %v", err)
	}
	matchID, ok := rt.MatchID(sid)
	if !ok {
		t.Fatal("the table has no match id")
	}
	// Bound to this table, so it cannot sign for another.
	if lk.Match() != matchID {
		t.Fatalf("the log key is bound to %q and the table is %q", lk.Match(), matchID)
	}

	// It is the log key, not the session key.
	session, logKey, err := rt.seatKeys(rt.Terms(sid).SID)
	if err != nil {
		t.Fatalf("seat keys: %v", err)
	}
	if !lk.Public().IsEqual(logKey.PubKey()) {
		t.Fatal("the key handed out is not this seat's log key")
	}
	if lk.Public().IsEqual(session.PubKey()) {
		t.Fatal("the session key was handed to the game")
	}

	// And every seat's log public key is readable, or nothing could check
	// what the others sign.
	logs, ok := rt.LogSeats(sid)
	if !ok || len(logs) != 2 {
		t.Fatalf("the table reports %d log keys", len(logs))
	}
	mine := ourSeatOf(t, rt, sid)
	if hex.EncodeToString(logs[mine]) != hex.EncodeToString(logKey.PubKey().SerializeCompressed()) {
		t.Fatal("this seat's log key is not the one the table lists for it")
	}
}
