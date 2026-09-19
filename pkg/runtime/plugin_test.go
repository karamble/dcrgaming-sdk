package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/decred/dcrd/chaincfg/v3"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/connect"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
	"github.com/karamble/dcrgaming-sdk/pkg/identity"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

// trivialGame is the smallest thing that can plug in: four methods, no
// dispatcher, no spend book, no seating machine of its own.
type trivialGame struct{ battleshipsRules }

// stand brings up a fake bridge and a runtime on it.
func stand(t *testing.T, g Rules) (*bridgetest.Bridge, *Runtime, context.CancelFunc) {
	t.Helper()
	fake := bridgetest.New(bridgetest.Options{
		Game: g.Identity().GameID, Network: "mainnet",
		Params: chaincfg.TestNet3Params(), Height: 800,
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
	seed, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	rt, err := Open(Config{
		Rules: g, Bridge: conn, Book: book,
		Identity: seed, SeatTags: testTags, Params: chaincfg.TestNet3Params(),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	go func() { _ = rt.Run(ctx) }()
	return fake, rt, cancel
}

// testTags stand in for a game's own frozen seat-key tags.
var testTags = identity.SeatTags{
	Session: "testgame/table-session/v1",
	Log:     "testgame/table-log/v1",
	Bond:    "testgame/bond/v1",
}

func TestARuntimeNeedsAGameABridgeAndSomewhereToWriteMoneyDown(t *testing.T) {
	book, err := spend.OpenBook(spend.MemStore())
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	seed, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	full := func(mut func(*Config)) Config {
		c := Config{Rules: &trivialGame{}, Bridge: &transport.Bridge{}, Book: book,
			Identity: seed, SeatTags: testTags, Params: chaincfg.TestNet3Params()}
		mut(&c)
		return c
	}
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"no identity", full(func(c *Config) { c.Identity = nil })},
		{"no seat tags", full(func(c *Config) { c.SeatTags = identity.SeatTags{} })},
		{"half the seat tags", full(func(c *Config) { c.SeatTags.Bond = "" })},
	} {
		if _, err := Open(tc.cfg); err == nil {
			t.Errorf("%s: built a runtime that could not derive a seat key", tc.name)
		}
	}
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"no game", full(func(c *Config) { c.Rules = nil })},
		{"no bridge", full(func(c *Config) { c.Bridge = nil })},
		{"no spend book", full(func(c *Config) { c.Book = nil })},
		{"a game that introduces nothing", full(func(c *Config) { c.Rules = &namelessGame{} })},
	} {
		if _, err := Open(tc.cfg); err == nil {
			t.Errorf("%s: built a runtime that could not work", tc.name)
		}
	}
}

type namelessGame struct{ battleshipsRules }

func (namelessGame) Identity() connect.Identity { return connect.Identity{} }

// The plug-in point: a game that implements four methods answers every
// the bridge's control requests, having written no dispatcher.
func TestAFourMethodGameAnswersAllControlRequests(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		req  *gamingpb.BridgeRequest
		ok   bool
	}{
		{"set the names", &gamingpb.BridgeRequest{
			RequestId: "r2",
			Req: &gamingpb.BridgeRequest_SetNames{
				SetNames: &gamingpb.SetNames{Names: map[string]string{"aa": "Ann"}},
			}}, true},
		{"report state", &gamingpb.BridgeRequest{
			RequestId: "r3",
			Req:       &gamingpb.BridgeRequest_RefreshState{RefreshState: &gamingpb.RefreshState{}},
		}, true},
		{"accept an invite", &gamingpb.BridgeRequest{
			RequestId: "r4",
			Req: &gamingpb.BridgeRequest_AcceptInvite{
				AcceptInvite: &gamingpb.AcceptInvite{Invite: invite(t, nil), Gcid: testGCID},
			}}, true},
		{"unknown", &gamingpb.BridgeRequest{
			RequestId: "r5",
		}, false},
	} {
		reply := &gamingpb.RespondRequest{RequestId: tc.req.GetRequestId()}
		err := rt.doRequest(ctx, tc.req, reply)
		switch {
		case tc.ok && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case !tc.ok && err == nil:
			t.Errorf("%s: answered when it should have refused", tc.name)
		}
	}

	if rt.Names()["aa"] != "Ann" {
		t.Errorf("the names were not kept: %v", rt.Names())
	}
}

// A request this runtime does not recognise is still answered, because a bridge
// left waiting is a dashboard stuck on a spinner.
func TestAnUnknownRequestIsRefusedRatherThanIgnored(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	reply := &gamingpb.RespondRequest{RequestId: "r9"}
	if err := rt.doRequest(context.Background(), &gamingpb.BridgeRequest{RequestId: "r9"}, reply); err == nil {
		t.Fatal("a request with no body was accepted")
	}
}

// The dashboard must still get an answer from a game whose own State panics.
func TestAPanickingGameStillReportsState(t *testing.T) {
	_, rt, _ := stand(t, &panickingGame{})
	st := rt.gameState(context.Background())
	if st == nil {
		t.Fatal("no state was reported at all")
	}
	if st.GetChainErr() == "" {
		t.Fatal("the panic was swallowed silently; an operator would see an empty dashboard")
	}
}

type panickingGame struct{ battleshipsRules }

func (panickingGame) State(context.Context) State { panic("the game fell over") }

// The read surface a game needs for its own duty clocks.
func TestAGameCanReadTheChainTip(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	got, err := rt.Chain(context.Background())
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	if got.Height != 800 || got.Hash == "" {
		t.Fatalf("tip is %d/%q", got.Height, got.Hash)
	}
	fake.Mine(5)
	if got, _ = rt.Chain(context.Background()); got.Height != 805 {
		t.Fatalf("after five blocks the tip is %d", got.Height)
	}
}

// Authorization bounds memory rather than checking identity: a sender may
// allocate reassembly state for a table this game is at, and no other. The
// sender is not checked at all, deliberately - see Runtime.authorized.
func TestOnlyATableThisGameIsAtMayAllocateState(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	if rt.authorized("no-such-table", "anyone") {
		t.Fatal("a stranger allocated state for a table this game is not at")
	}
	rt.mu.Lock()
	rt.tables["m1"] = &table{match: "m1"}
	rt.mu.Unlock()
	if !rt.authorized("m1", "anyone") {
		t.Fatal("a message for a table this game is at was refused")
	}
	if !rt.authorized("m1", "") {
		t.Fatal("the sender is meant to be ignored, and was not")
	}
}

func TestSettleRefusesAnOutcomeThatDecidesNothing(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	ctx := context.Background()
	if err := rt.Settle(ctx, "", Outcome{Void: true}); err == nil {
		t.Fatal("settled a table that was not named")
	}
	if err := rt.Settle(ctx, "m1", Outcome{}); err == nil {
		t.Fatal("settled an outcome that is neither void nor a share of anything")
	}
	if err := rt.Settle(ctx, "m1", Outcome{Void: true}); err == nil {
		t.Fatal("settled a table this game is not at")
	}
}

// The loop stops when its context does, rather than leaking goroutines into
// whatever runs next.
func TestTheLoopStopsWithItsContext(t *testing.T) {
	_, _, cancel := stand(t, &trivialGame{})
	cancel()
	time.Sleep(50 * time.Millisecond)
}

// A reply is always sent, including for a request that failed and one nobody
// recognises. This drives answer rather than doRequest, because the guarantee
// is answer's.
func TestEveryRequestIsAnsweredEvenWhenItFails(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	ctx := context.Background()

	rt.answer(ctx, &gamingpb.BridgeRequest{
		RequestId: "ok1",
		Req:       &gamingpb.BridgeRequest_RefreshState{RefreshState: &gamingpb.RefreshState{}},
	})
	rt.answer(ctx, &gamingpb.BridgeRequest{
		RequestId: "fail1",
	})
	rt.answer(ctx, &gamingpb.BridgeRequest{RequestId: "unknown1"})

	got := map[string]*gamingpb.RespondRequest{}
	for _, rep := range fake.Replies() {
		got[rep.GetRequestId()] = rep
	}
	for _, id := range []string{"ok1", "fail1", "unknown1"} {
		if _, ok := got[id]; !ok {
			t.Errorf("%s was never answered; the dashboard would hang on it", id)
		}
	}
	if r := got["ok1"]; r != nil && !r.GetOk() {
		t.Error("a request that succeeded was reported as failed")
	}
	for _, id := range []string{"fail1", "unknown1"} {
		r := got[id]
		if r == nil {
			continue
		}
		if r.GetOk() {
			t.Errorf("%s failed but was reported as fine", id)
		}
		if r.GetError() == "" {
			t.Errorf("%s failed without saying why", id)
		}
	}
}

// The lifecycle's own traffic is the runtime's, and never reaches the game. A
// game that saw a join would be a game that had to know what one was.
func TestTheRuntimeKeepsItsOwnMessagesFromTheGame(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	for _, k := range []schema.Kind{schema.KindJoin, schema.KindCommit} {
		if !rt.ours(k) {
			t.Errorf("%s was left to the game", k)
		}
	}
	for _, k := range []schema.Kind{"place", "shoot", "shuffle", schema.KindAction} {
		if rt.ours(k) {
			t.Errorf("%s was taken from the game", k)
		}
	}
}

// A game's message reaches the game; a runtime message does not.
func TestAGamesOwnMessageReachesIt(t *testing.T) {
	_, rt, _ := stand(t, &countingGame{})
	g := rt.rules.(*countingGame)

	rt.deliver(transport.Delivery{SID: "abcdef01", Msg: &schema.Message{Kind: "shoot"}})
	if g.seen != 1 {
		t.Fatalf("the game saw %d of its own messages", g.seen)
	}
	rt.deliver(transport.Delivery{SID: "abcdef01", Msg: &schema.Message{Kind: schema.KindJoin}})
	if g.seen != 1 {
		t.Fatalf("a join reached the game (%d seen)", g.seen)
	}
}

type countingGame struct {
	battleshipsRules
	seen int
}

func (c *countingGame) Handle(context.Context, Message) error { c.seen++; return nil }

// A request that ran past the console's deadline is still answered. It is the
// one whose answer matters most - something slow happened - and sending the
// reply on the expired context would fail before it left the process.
func TestAnAnswerOutlivesTheDeadlineTheRequestCarried(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	req := &gamingpb.BridgeRequest{
		RequestId:    "late",
		DeadlineUnix: 1, // 1970: long gone
		Req:          &gamingpb.BridgeRequest_RefreshState{RefreshState: &gamingpb.RefreshState{}},
	}
	rt.answer(context.Background(), req)

	var got *gamingpb.RespondRequest
	for _, rep := range fake.Replies() {
		if rep.GetRequestId() == "late" {
			got = rep
		}
	}
	if got == nil {
		t.Fatal("the request went unanswered, so the console waits forever")
	}
}

// The state that answers a refresh names the refresh it answers. The proto says
// request_id is set only then, so without it the console cannot tell this from
// an unsolicited report and has no way to retire the one it asked for.
func TestAStateReplyNamesTheRefreshItAnswers(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	reply := &gamingpb.RespondRequest{RequestId: "r7"}
	err := rt.doRequest(context.Background(), &gamingpb.BridgeRequest{
		RequestId: "r7",
		Req:       &gamingpb.BridgeRequest_RefreshState{RefreshState: &gamingpb.RefreshState{}},
	}, reply)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	st, ok := reply.GetResult().(*gamingpb.RespondRequest_State)
	if !ok {
		t.Fatalf("a refresh answered with %T", reply.GetResult())
	}
	if st.State.GetRequestId() != "r7" {
		t.Fatalf("the state names %q, not the refresh that asked for it", st.State.GetRequestId())
	}
}

// Game-control work is bounded by the console's deadline. Financial work is
// performed by the bridge and never enters this dispatcher.
func TestEveryGameControlRequestUsesTheConsolesDeadline(t *testing.T) {
	soon := time.Now().Add(time.Hour).Unix()
	at := func(r *gamingpb.BridgeRequest) *gamingpb.BridgeRequest {
		r.DeadlineUnix = soon
		return r
	}
	for _, tc := range []struct {
		name string
		req  *gamingpb.BridgeRequest
	}{
		{"accept an invite", at(&gamingpb.BridgeRequest{Req: &gamingpb.BridgeRequest_AcceptInvite{
			AcceptInvite: &gamingpb.AcceptInvite{}}})},
		{"report state", at(&gamingpb.BridgeRequest{Req: &gamingpb.BridgeRequest_RefreshState{
			RefreshState: &gamingpb.RefreshState{}}})},
		{"set the names", at(&gamingpb.BridgeRequest{Req: &gamingpb.BridgeRequest_SetNames{
			SetNames: &gamingpb.SetNames{}}})},
	} {
		ctx, cancel := ctxFor(context.Background(), tc.req)
		_, has := ctx.Deadline()
		cancel()
		if !has {
			t.Errorf("%s: request ignored the console deadline", tc.name)
		}
	}
}

// The dashboard is the runtime's to fill. A game that says nothing about its
// tables still gives an operator the chat, the buy-in and the deadline, because
// the runtime knows them and the game would only be repeating itself.
func TestTheRuntimeReportsATableWithoutTheGamesHelp(t *testing.T) {
	_, rt, _ := stand(t, &silentGame{})
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	st := rt.gameState(context.Background())
	if len(st.GetTables()) != 1 {
		t.Fatalf("reported %d tables", len(st.GetTables()))
	}
	row := st.GetTables()[0]
	if row.GetSid() != sid {
		t.Errorf("reported table %q, not %q", row.GetSid(), sid)
	}
	if row.GetGcid() != testGCID {
		t.Errorf("reported chat %q, not %q", row.GetGcid(), testGCID)
	}
	if row.GetState() == "" {
		t.Error("reported no phase")
	}
	if row.GetSeats() == 0 || row.GetBuyinAtoms() == 0 || row.GetUntil() == 0 {
		t.Errorf("terms missing: seats %d, buy-in %d, until %d",
			row.GetSeats(), row.GetBuyinAtoms(), row.GetUntil())
	}
}

// silentGame implements Rules and nothing else, which is the whole point.
type silentGame struct{ battleshipsRules }

// A game that does implement Reporting replaces the status line, and only that.
func TestAGameCanReplaceTheStatusLine(t *testing.T) {
	g := &talkativeGame{}
	_, rt, _ := stand(t, g)
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	g.match = sid
	st := rt.gameState(context.Background())
	if len(st.GetTables()) != 1 {
		t.Fatalf("reported %d tables", len(st.GetTables()))
	}
	if got := st.GetTables()[0].GetState(); got != "waiting for a shot" {
		t.Errorf("status line is %q; the game's was not used", got)
	}
	if st.GetTables()[0].GetGcid() != testGCID {
		t.Error("the runtime's own fields were lost when the game spoke")
	}
}

type talkativeGame struct {
	battleshipsRules
	match string
}

func (g *talkativeGame) State(context.Context) State {
	return State{Tables: []TableState{{Match: g.match, Status: "waiting for a shot"}}}
}
