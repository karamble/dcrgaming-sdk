package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/decred/dcrd/chaincfg/v3"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/connect"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
	"github.com/karamble/dcrgaming-sdk/pkg/ruling"
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
	rt, err := New(Config{Rules: g, Bridge: conn, Book: book})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	go func() { _ = rt.Run(ctx) }()
	return fake, rt, cancel
}

func TestARuntimeNeedsAGameABridgeAndSomewhereToWriteMoneyDown(t *testing.T) {
	book, err := spend.OpenBook(spend.MemStore())
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"no game", Config{Book: book}},
		{"no bridge", Config{Rules: &trivialGame{}, Book: book}},
		{"no spend book", Config{Rules: &trivialGame{}, Bridge: &transport.Bridge{}}},
		{"a game that introduces nothing", Config{Rules: &namelessGame{}, Bridge: &transport.Bridge{}, Book: book}},
	} {
		if _, err := New(tc.cfg); err == nil {
			t.Errorf("%s: built a runtime that could not work", tc.name)
		}
	}
}

type namelessGame struct{ battleshipsRules }

func (namelessGame) Identity() connect.Identity { return connect.Identity{} }

// The plug-in point: a game that implements four methods answers all five of
// the bridge's control requests, having written no dispatcher.
func TestAFourMethodGameAnswersAllFiveControlRequests(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		req  *gamingpb.BridgeRequest
		ok   bool
	}{
		{"set the payout address", &gamingpb.BridgeRequest{
			RequestId: "r1",
			Req: &gamingpb.BridgeRequest_SetPayout{
				SetPayout: &gamingpb.SetPayoutAddress{Address: "TsPayout"},
			}}, true},
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
				AcceptInvite: &gamingpb.AcceptInvite{Invite: "inv", Gcid: strings.Repeat("a", 64)},
			}}, false},
		{"reclaim", &gamingpb.BridgeRequest{
			RequestId: "r5",
			Req: &gamingpb.BridgeRequest_Reclaim{
				Reclaim: &gamingpb.Reclaim{Kind: gamingpb.Reclaim_BOND, Sid: "s", DestAddr: "TsHome"},
			}}, false},
	} {
		reply := &gamingpb.RespondRequest{RequestId: tc.req.GetRequestId()}
		err := rt.doRequest(ctx, tc.req, reply)
		switch {
		case tc.ok && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case !tc.ok && err == nil:
			t.Errorf("%s: answered a stage that is not built", tc.name)
		case !tc.ok && !errors.Is(err, ErrNotYet):
			t.Errorf("%s: failed for the wrong reason: %v", tc.name, err)
		}
	}

	if rt.Payout() != "TsPayout" {
		t.Errorf("the payout address was not kept: %q", rt.Payout())
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
	if got.Height != 1000 || got.Hash == "" {
		t.Fatalf("tip is %d/%q", got.Height, got.Hash)
	}
	fake.Mine(5)
	if got, _ = rt.Chain(context.Background()); got.Height != 1005 {
		t.Fatalf("after five blocks the tip is %d", got.Height)
	}
}

// A stranger cannot talk to a table they are not seated at, checked before any
// state is allocated for them.
func TestAStrangerIsNotAuthorisedAtATableTheyAreNotAt(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	if rt.authorized("no-such-table", "stranger") {
		t.Fatal("a stranger was authorised at a table that does not exist")
	}
	rt.mu.Lock()
	rt.tables["m1"] = &table{match: "m1", senders: []string{"friend"}}
	rt.mu.Unlock()
	if rt.authorized("m1", "stranger") {
		t.Fatal("a stranger was authorised at a table they are not seated at")
	}
	if !rt.authorized("m1", "friend") {
		t.Fatal("a seated player was refused")
	}
}

// A malformed ruling is refused before the stage that would spend on it exists,
// so a game integrating now finds out now.
func TestAMalformedRulingIsRefusedBeforeAnythingIsSpent(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	ctx := context.Background()

	if err := rt.Forfeit(ctx, ruling.Ruling{Kind: ruling.Equivocation}); err == nil {
		t.Fatal("accepted an equivocation ruling with no proof")
	}
	err := rt.Forfeit(ctx, ruling.Ruling{Match: "m1", Kind: ruling.Clean})
	if !errors.Is(err, ErrNotYet) {
		t.Fatalf("a well-formed ruling failed for the wrong reason: %v", err)
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
	if err := rt.Settle(ctx, "m1", Outcome{Void: true}); !errors.Is(err, ErrNotYet) {
		t.Fatalf("a void outcome failed for the wrong reason: %v", err)
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
		Req: &gamingpb.BridgeRequest_Reclaim{
			Reclaim: &gamingpb.Reclaim{Kind: gamingpb.Reclaim_BOND, Sid: "s", DestAddr: "TsHome"},
		},
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
