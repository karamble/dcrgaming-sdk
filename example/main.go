// Command example is the shortest complete integration of dcrgaming-sdk.
//
// It plays one table against pkg/gaming/bridgetest, so it needs no wallet, no
// bridge and no network. Everything here that is not the game itself is what a
// real game also writes; if this file grows, the SDK got harder to use.
//
//	go run ./example
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/decred/dcrd/chaincfg/v3"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/connect"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
	"github.com/karamble/dcrgaming-sdk/pkg/identity"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
	"github.com/karamble/dcrgaming-sdk/pkg/runtime"
)

// game is the whole of what a game has to provide: three methods.
type game struct {
	rt *runtime.Runtime
}

// Identity is what the bridge routes on. It checks this rather than believing it.
func (g *game) Identity() connect.Identity {
	return connect.Identity{
		GameID:          "example",
		GameVer:         1,
		ClientVersion:   "example/0.1",
		MinRefundBlocks: 288,
		BondLockBlocks:  288,
	}
}

// Terms is what a table costs. Returning an error refuses the table.
func (g *game) Terms(sid string) (membership.Terms, error) {
	tip, err := g.rt.Chain(context.Background())
	if err != nil {
		return membership.Terms{}, err
	}
	return membership.Terms{
		Game: "example", GameVer: 1, SID: sid,
		Seats: 2, BuyInAtoms: 100_000,
		CSVBlocks:      288,
		Until:          uint32(tip.Height) + 16,
		BondAtoms:      1_000_000,
		BondLockBlocks: 288,
	}, nil
}

// Handle is one message from the other seat. The runtime has already framed,
// routed, reassembled and authenticated it; the body is whatever we put there.
func (g *game) Handle(_ context.Context, in runtime.Message) error {
	fmt.Printf("  move from %s: %s\n", in.From[:8], in.Body)
	return nil
}

// Seated is optional. Fund blocks until a person approves, so it is started
// rather than called.
func (g *game) Seated(ctx context.Context, match string, _ map[uint32][]byte) {
	go func() {
		if err := g.rt.Fund(ctx, match); err != nil {
			fmt.Println("  fund:", err)
			return
		}
		fmt.Println("  stake is in escrow")
	}()
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "example:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	g := &game{}

	// A real game dials a dcrpulse bridge here instead. Everything after this
	// is identical either way.
	fake := bridgetest.New(bridgetest.Options{
		Game: "example", Network: "testnet3",
		Params: chaincfg.TestNet3Params(), Height: 800,
	})
	srv, err := fake.Serve("seat0")
	if err != nil {
		return err
	}
	defer srv.Close()
	bridge, err := srv.Dial(ctx, "seat0", func(cfg *transport.BridgeConfig) {
		connect.Stamp(cfg, g.Identity())
	})
	if err != nil {
		return err
	}
	defer bridge.Close()

	seed, err := identity.Load(mustTempDir())
	if err != nil {
		return err
	}

	rt, err := runtime.Open(runtime.Config{
		Rules:    g,
		Bridge:   bridge,
		Identity: seed,
		Dir:      mustTempDir(),
		Params:   chaincfg.TestNet3Params(), // the fake has not said hello
		SeatTags: identity.SeatTags{
			Session: "Example/session/v1",
			Log:     "Example/log/v1",
			Bond:    "Example/bond/v1",
		},
	})
	if err != nil {
		return err
	}
	defer rt.Close()
	g.rt = rt

	go rt.Run(ctx)

	// From here on it is the game. A table arrives when the operator creates
	// one in dcrpulse; Seated starts the funding, Handle receives the other
	// seat's moves, and the game sends its own:
	//
	//	rt.Send(ctx, match, "example.move", body, wire.ClassTurn)
	//	rt.Settle(ctx, match, runtime.Outcome{Shares: map[uint32]int64{0: pot}})
	//
	// then polls RefreshDeposits until this seat's stake reads "spent".
	fmt.Println("runtime is up and following the chain")
	return nil
}

func mustTempDir() string {
	dir, err := os.MkdirTemp("", "dcrgaming-example-")
	if err != nil {
		panic(err)
	}
	return dir
}
