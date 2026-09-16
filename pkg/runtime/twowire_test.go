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
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

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

func wireSeat(t *testing.T, ctx context.Context, srv *bridgetest.Server, seat string, rules Rules) *Runtime {
	t.Helper()
	conn, err := srv.Dial(ctx, seat, func(cfg *transport.BridgeConfig) {
		connect.Stamp(cfg, rules.Identity())
	})
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
	rt, err := New(Config{
		Rules: rules, Bridge: conn, Book: book, Tables: NewMemTableStore(),
		Identity: seed, SeatTags: testTags, Params: chaincfg.TestNet3Params(),
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	return rt
}

type wirePair struct {
	fake     *bridgetest.Bridge
	one, two *Runtime
	ctx      context.Context
	sid      string
}

func seatedWirePair(t *testing.T, second Rules) wirePair {
	t.Helper()
	fake := bridgetest.New(bridgetest.Options{
		Game: "battleships", Network: "mainnet",
		Params: chaincfg.TestNet3Params(), Height: 700,
	})
	srv, err := fake.Serve("seat0", "seat1")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if second == nil {
		second = &trivialGame{}
	}
	one := wireSeat(t, ctx, srv, "seat0", &trivialGame{})
	two := wireSeat(t, ctx, srv, "seat1", second)
	go func() { _ = one.Run(ctx) }()
	go func() { _ = two.Run(ctx) }()
	waitFor(t, "both seats to subscribe", func() bool { return fake.Subscribers() == 2 })

	link := invite(t, nil)
	sid, err := accept(one, link, testGCID)
	if err != nil {
		t.Fatalf("first seat accepting: %v", err)
	}
	if _, err = accept(two, link, testGCID); err != nil {
		t.Fatalf("second seat accepting: %v", err)
	}
	pair := wirePair{fake: fake, one: one, two: two, ctx: ctx, sid: sid}
	waitFor(t, "both peers to seat the same table", func() bool {
		pair.block()
		_, a := one.Seats(sid)
		_, b := two.Seats(sid)
		return a && b
	})
	return pair
}

func (p wirePair) block() {
	p.fake.Mine(1)
	p.one.Tick(p.ctx, p.fake.Height())
	p.two.Tick(p.ctx, p.fake.Height())
}

func TestTwoRuntimesSeatEachOtherOverTheWire(t *testing.T) {
	p := seatedWirePair(t, nil)
	first, _ := p.one.Seats(p.sid)
	second, _ := p.two.Seats(p.sid)
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("the peers seated %d and %d players", len(first), len(second))
	}
	for seat, key := range first {
		if !bytesEqual(key, second[seat]) {
			t.Fatalf("the peers disagree about seat %d", seat)
		}
	}
	a, aok := p.one.Seat(p.sid)
	b, bok := p.two.Seat(p.sid)
	if !aok || !bok || a == b {
		t.Fatalf("the peers took seats %d/%v and %d/%v", a, aok, b, bok)
	}
}

func TestCooperativePayoutNeedsBothBridgeApprovals(t *testing.T) {
	p := seatedWirePair(t, nil)
	for name, rt := range map[string]*Runtime{"one": p.one, "two": p.two} {
		if err := rt.Fund(p.ctx, p.sid); err != nil {
			t.Fatalf("%s funding its stake: %v", name, err)
		}
	}
	waitFor(t, "both peers to learn both stakes", func() bool {
		p.block()
		return fundedByEverySeat(p.one, p.sid) && fundedByEverySeat(p.two, p.sid)
	})

	winner, _ := p.one.Seat(p.sid)
	pot := int64(p.one.Terms(p.sid).BuyInAtoms) * 2
	out := Outcome{Shares: map[uint32]int64{winner: pot}}
	if err := p.one.Settle(p.ctx, p.sid, out); err != nil {
		t.Fatalf("first payout proposal: %v", err)
	}
	if err := p.two.Settle(p.ctx, p.sid, out); err != nil {
		t.Fatalf("second payout proposal: %v", err)
	}
	if sent := p.fake.Broadcasts(); len(sent) != 0 {
		t.Fatalf("payout broadcast before dashboard approval: %v", sent)
	}
	if err := p.fake.SetPayoutVerdict(bridgetest.Approve); err != nil {
		t.Fatalf("approve payout: %v", err)
	}
	sent := p.fake.Broadcasts()
	if len(sent) != 1 {
		t.Fatalf("broadcast %d payouts after both approvals, want one: %v", len(sent), sent)
	}

	var payoutID string
	for name, rt := range map[string]*Runtime{"one": p.one, "two": p.two} {
		rt.mu.Lock()
		id := rt.tables[p.sid].payoutID
		rt.mu.Unlock()
		if id == "" {
			t.Fatalf("%s retained no authoritative payout id", name)
		}
		if payoutID != "" && payoutID != id {
			t.Fatalf("the peers proposed different payouts: %s and %s", payoutID, id)
		}
		payoutID = id
		status, err := rt.bridge.PayoutStatus(p.ctx, id)
		if err != nil || status.GetState() != "confirmed" || status.GetTxid() == "" {
			t.Fatalf("%s payout status: %+v, %v", name, status, err)
		}
		snapshot, err := rt.RefreshDeposits(p.ctx, p.sid)
		if err != nil {
			t.Fatalf("%s refresh after payout: %v", name, err)
		}
		seenOwnSpend := false
		for _, deposit := range snapshot.Deposits {
			if deposit.AuthorityID != "" && deposit.Purpose == "stake" {
				seenOwnSpend = deposit.AuthorityState == "spent" && deposit.Check == "spent"
			}
		}
		if !seenOwnSpend {
			t.Fatalf("%s did not refresh its bridge-owned stake after payout: %+v", name, snapshot.Deposits)
		}
	}
}

func fundedByEverySeat(rt *Runtime, sid string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	t := rt.tables[sid]
	if t == nil || t.formation() == nil {
		return false
	}
	seats, ok := t.formation().Seats()
	return ok && len(t.funded) == len(seats)
}

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

func TestAGamesOwnMessageCrossesAndFinancialMessagesCannotBeForged(t *testing.T) {
	heard := make(chan Message, 8)
	p := seatedWirePair(t, &listeningRules{battleshipsRules: &battleshipsRules{}, heard: heard})
	if err := p.one.Send(p.ctx, p.sid, "shoot", map[string]any{"x": 3, "y": 4}, wire.ClassTurn); err != nil {
		t.Fatalf("send a shot: %v", err)
	}
	select {
	case got := <-heard:
		var body struct{ X, Y int }
		if got.Kind != "shoot" || got.Match != p.sid || json.Unmarshal(got.Body, &body) != nil || body.X != 3 || body.Y != 4 {
			t.Fatalf("unexpected game message: %+v / %+v", got, body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the other peer never received the game message")
	}

	for _, kind := range []schema.Kind{
		schema.KindJoin, schema.KindCommit, KindRoster, KindFunded, KindBonded,
		KindPayout, KindResync, KindResyncReply,
	} {
		err := p.one.Send(p.ctx, p.sid, kind, map[string]any{}, wire.ClassTurn)
		if err == nil || !strings.Contains(err.Error(), "not a game's to send") {
			t.Errorf("forging %q: %v", kind, err)
		}
	}
}

var _ Game = (*Runtime)(nil)

func TestAGameIsHandedItsLogKeyAndNoFinancialKey(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := seatTwo(t, fake, rt)
	lk, err := rt.LogKey(sid)
	if err != nil {
		t.Fatalf("log key: %v", err)
	}
	matchID, ok := rt.MatchID(sid)
	if !ok || lk.Match() != matchID {
		t.Fatalf("log key is bound to %q, table is %q/%v", lk.Match(), matchID, ok)
	}
	session, logKey, err := rt.seatKeys(rt.Terms(sid).SID)
	if err != nil {
		t.Fatalf("seat keys: %v", err)
	}
	if !lk.Public().IsEqual(logKey.PubKey()) || lk.Public().IsEqual(session.PubKey()) {
		t.Fatal("game did not receive exactly its gameplay log key")
	}
	logs, ok := rt.LogSeats(sid)
	if !ok || len(logs) != 2 {
		t.Fatalf("the table reports %d log keys", len(logs))
	}
	mine := ourSeatOf(t, rt, sid)
	if hex.EncodeToString(logs[mine]) != hex.EncodeToString(logKey.PubKey().SerializeCompressed()) {
		t.Fatal("this seat's log key differs from the roster")
	}
}
