package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/decred/dcrd/chaincfg/v3"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/connect"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
	"github.com/karamble/dcrgaming-sdk/pkg/identity"
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
				t.Logf("%s: state=%v joins=%d agreed=%v seats=%d/%v closed=%v",
					name, tbl.form.State(), len(tbl.form.Joins()), tbl.form.Agreed(),
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
