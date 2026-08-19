package runtime

import (
	"context"
	"fmt"
	"testing"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/connect"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

// pokerRules is dcrpoker's shape: no bond terms of its own, a hand-sized refund
// lock, and a dealing vocabulary. Modelled on cmd/dcrpoker.
type pokerRules struct{ hands int }

func (p *pokerRules) Identity() connect.Identity {
	return connect.Identity{
		GameID: "poker", GameVer: 5, ClientVersion: "dcrpoker",
		Capabilities: []gamingpb.Capability{
			gamingpb.Capability_CAP_ACCEPT_INVITE,
			gamingpb.Capability_CAP_RECLAIM,
			gamingpb.Capability_CAP_SET_PAYOUT,
			gamingpb.Capability_CAP_SET_NAMES,
		},
	}
}

func (p *pokerRules) Terms(sid string) (membership.Terms, error) {
	return membership.Terms{
		Game: "poker", GameVer: 5, SID: sid,
		BuyInAtoms: 5_000_000, Seats: 2, CSVBlocks: 288, Until: 900,
	}, nil
}

func (p *pokerRules) Handle(_ context.Context, in Message) error {
	switch in.Kind {
	case "shuffle", "share", "cardkey", "action", "checkpoint", "reveal":
		return nil
	}
	return fmt.Errorf("poker does not know %q", in.Kind)
}

func (p *pokerRules) State(context.Context) State {
	return State{Summary: fmt.Sprintf("%d hands played", p.hands)}
}

// battleshipsRules is dcrbattleships' shape: bond terms of its own, a much
// longer refund lock, and a placement vocabulary. Modelled on
// cmd/dcrbattleshipsd and pkg/match.
type battleshipsRules struct{ shots int }

func (b *battleshipsRules) Identity() connect.Identity {
	return connect.Identity{
		GameID: "battleships", GameVer: 1, ClientVersion: "dcrbattleshipsd",
		Capabilities: []gamingpb.Capability{
			gamingpb.Capability_CAP_ACCEPT_INVITE,
			gamingpb.Capability_CAP_RECLAIM,
			gamingpb.Capability_CAP_SET_PAYOUT,
			gamingpb.Capability_CAP_SET_NAMES,
		},
		MinRefundBlocks: 2048, BondLockBlocks: 4032,
	}
}

func (b *battleshipsRules) Terms(sid string) (membership.Terms, error) {
	return membership.Terms{
		Game: "battleships", GameVer: 1, SID: sid,
		BuyInAtoms: 5_000_000, Seats: 2, CSVBlocks: 2048, Until: 900,
		BondAtoms: escrow.MinBondAtoms, BondLockBlocks: 4032,
	}, nil
}

func (b *battleshipsRules) Handle(_ context.Context, in Message) error {
	switch in.Kind {
	case "place", "shoot", "answer", "punishkey", "reveal":
		return nil
	}
	return fmt.Errorf("battleships does not know %q", in.Kind)
}

func (b *battleshipsRules) State(context.Context) State {
	return State{Summary: fmt.Sprintf("%d shots fired", b.shots)}
}

// battleships also takes the optional hooks; poker deliberately does not, which
// is what makes them optional rather than part of Rules.
func (b *battleshipsRules) Seated(context.Context, string, map[uint32][]byte) {}
func (b *battleshipsRules) Settled(context.Context, string, string)           {}

// The spike's decision rule, as a compile-time fact: both games satisfy the
// same interface, and neither needs a method the other does not.
var (
	_ Rules   = (*pokerRules)(nil)
	_ Rules   = (*battleshipsRules)(nil)
	_ Seated  = (*battleshipsRules)(nil)
	_ Settled = (*battleshipsRules)(nil)
)

// Terms differ sharply between the two games - poker states no bond and a
// 288-block refund lock, battleships states a bond and 2048 - and both must go
// through the one method without the runtime branching on which game it is.
func TestBothGamesStateTheirTermsThroughOneMethod(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules Rules
		bond  bool
	}{
		{"poker", &pokerRules{}, false},
		{"battleships", &battleshipsRules{}, true},
	} {
		tm, err := tc.rules.Terms("sid-abc")
		if err != nil {
			t.Fatalf("%s: terms: %v", tc.name, err)
		}
		if err := tm.Validate(); err != nil {
			t.Errorf("%s: the game stated terms no table could use: %v", tc.name, err)
		}
		if got := tm.BondAtoms != 0; got != tc.bond {
			t.Errorf("%s: states a bond = %v, want %v", tc.name, got, tc.bond)
		}
		if _, err := tm.Hash(); err != nil {
			t.Errorf("%s: terms do not hash: %v", tc.name, err)
		}
	}
}

// The runtime never looks inside a body, so two games with entirely unrelated
// vocabularies both work and neither can see the other's.
func TestEachGameKeepsItsOwnVocabulary(t *testing.T) {
	ctx := context.Background()
	poker, ships := &pokerRules{}, &battleshipsRules{}

	if err := poker.Handle(ctx, Message{Kind: schema.Kind("shuffle")}); err != nil {
		t.Errorf("poker refused its own message: %v", err)
	}
	if err := ships.Handle(ctx, Message{Kind: schema.Kind("shoot")}); err != nil {
		t.Errorf("battleships refused its own message: %v", err)
	}
	if err := poker.Handle(ctx, Message{Kind: schema.Kind("shoot")}); err == nil {
		t.Error("poker accepted a battleships message")
	}
	if err := ships.Handle(ctx, Message{Kind: schema.Kind("shuffle")}); err == nil {
		t.Error("battleships accepted a poker message")
	}
}

// Identity is the one thing every game must state and no game can share.
func TestEachGameIntroducesItself(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules Rules
	}{
		{"poker", &pokerRules{}},
		{"battleships", &battleshipsRules{}},
	} {
		id := tc.rules.Identity()
		if err := id.Validate(); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if id.GameID != tc.name {
			t.Errorf("%s introduces itself as %q", tc.name, id.GameID)
		}
	}
}

// A game with no tables still has to answer the dashboard, because the bridge
// asks whether or not the game feels ready.
func TestStateIsAnsweredEvenWithNothingHappening(t *testing.T) {
	for _, r := range []Rules{&pokerRules{}, &battleshipsRules{}} {
		if got := r.State(context.Background()); got.Summary == "" {
			t.Errorf("%s answered the dashboard with nothing", r.Identity().GameID)
		}
	}
}
