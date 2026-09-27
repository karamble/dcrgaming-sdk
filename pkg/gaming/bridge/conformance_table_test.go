// Copyright (c) 2015-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package bridge

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/decred/dcrd/chaincfg/chainhash"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/connect"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
	"github.com/karamble/dcrgaming-sdk/pkg/identity"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
	"github.com/karamble/dcrgaming-sdk/pkg/runtime"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

// confRules is a game with no rules of its own: it sits at any table it is
// invited to and plays nothing, so a table runs only the SDK's own protocol.
type confRules struct{}

func (confRules) Identity() connect.Identity {
	return connect.Identity{
		GameID: confGame, GameVer: 1, ClientVersion: "conformance",
		Capabilities: []gamingpb.Capability{gamingpb.Capability_CAP_ACCEPT_INVITE},
	}
}

func (confRules) Terms(sid string) (membership.Terms, error) {
	return membership.Terms{Game: confGame, GameVer: 1, SID: sid}, nil
}

func (confRules) Handle(context.Context, runtime.Message) error { return nil }

// confPlayer is one wallet app's bridge with the SDK runtime connected to it.
type confPlayer struct {
	host *fakeHost
	br   *Bridge
	rt   *runtime.Runtime
}

func newConfPlayer(t *testing.T, ctx context.Context, chain *fakeChain, name string) *confPlayer {
	t.Helper()
	h, br, cfg := startConfBridge(t, chain, name, confPolicy())
	rules := confRules{}
	connect.Stamp(&cfg, rules.Identity())
	conn, err := transport.Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	book, err := spend.OpenBook(spend.MemStore())
	if err != nil {
		t.Fatal(err)
	}
	seed, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rt, err := runtime.Open(runtime.Config{
		Rules: rules, Bridge: conn, Book: book, Tables: runtime.NewMemTableStore(),
		Identity: seed, Params: fakeParams, TickEvery: -1,
		SeatTags: identity.SeatTags{Session: "confgame/session/v1", Log: "confgame/log/v1", Bond: "confgame/bond/v1"},
	})
	if err != nil {
		t.Fatalf("open the runtime: %v", err)
	}
	go func() { _ = rt.Run(ctx) }()
	confEventually(t, name+"'s game to connect", func() bool { return br.gamingGameConnected(confGame) })
	return &confPlayer{host: h, br: br, rt: rt}
}

// approvePending is the operator approving every payment waiting on them.
func (p *confPlayer) approvePending(t *testing.T, ctx context.Context) int {
	t.Helper()
	spends, _, err := p.br.GamingSpendLedger()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range spends {
		if s.State != GamingSpendPending {
			continue
		}
		if _, err := p.br.ApproveGamingSpend(ctx, s.ID, []byte(p.host.wallet.pass)); err != nil {
			t.Fatalf("approve %s: %v", s.Reason, err)
		}
		n++
	}
	return n
}

// confTable is two wallet apps at one table, each with its own bridge, wallet
// and Bison Relay client, on one chain and one group chat.
type confTable struct {
	chain      *fakeChain
	alice, bob *confPlayer
	gcid       string
	sid        string
	until      int64
}

func (c *confTable) players() []*confPlayer { return []*confPlayer{c.alice, c.bob} }

// tick is both bridges and both games reading the chain as it stands, the
// bridges' part being the reconcile pass their financial worker runs every
// thirty seconds.
func (c *confTable) tick(ctx context.Context) {
	for _, p := range c.players() {
		p.br.reconcileGamingFinance(ctx)
		p.rt.Tick(ctx, c.chain.height())
	}
}

// waitFor ticks until done, mining a block now and then, but never past
// ceiling: a table's deadlines are heights, and a test racing the chain past
// one would be testing that the deadline works.
func (c *confTable) waitFor(t *testing.T, ctx context.Context, what string, ceiling int64, done func() bool) {
	t.Helper()
	polls := 0
	confEventually(t, what, func() bool {
		c.tick(ctx)
		if done() {
			return true
		}
		if polls++; polls%25 == 0 && c.chain.height() < ceiling {
			c.chain.mine(1)
		}
		return false
	})
}

// bound reports whether p's bridge has bound the table's seats.
func (c *confTable) bound(ctx context.Context, p *confPlayer) bool {
	scope, err := p.br.gamingFinancialScope(ctx, confGame)
	if err != nil {
		return false
	}
	store, err := p.br.gamingFundsStore()
	if err != nil {
		return false
	}
	peers, err := store.Participants(scope, c.sid)
	return err == nil && len(peers) == 2
}

// waiting reports whether p's bridge holds a financial frame from sender that
// it could not apply yet.
func (c *confTable) waiting(p, sender *confPlayer) bool {
	uid := hex.EncodeToString(sender.host.relay.uid[:])
	return slices.ContainsFunc(p.br.financialReplay(), func(ev GamingFrameEvent) bool { return ev.From == uid })
}

func (c *confTable) phase(p *confPlayer) string {
	snap, err := p.rt.Snapshot(c.sid)
	if err != nil {
		return ""
	}
	return snap.Phase
}

// seatConfTable has alice create a table in a group chat she and bob share,
// bob accept it, and both operators pay their seat bonds, until both games
// hold the same seated roster.
func seatConfTable(t *testing.T, ctx context.Context) *confTable {
	t.Helper()
	chain := newFakeChain(1000)
	c := &confTable{chain: chain}
	c.alice = newConfPlayer(t, ctx, chain, "alice")
	c.bob = newConfPlayer(t, ctx, chain, "bob")
	c.alice.host.relay.joinGroup(ctx, c.bob.br)
	c.bob.host.relay.joinGroup(ctx, c.alice.br)
	c.gcid, _ = confGCID(0xab)

	table, err := c.alice.br.CreateGamingTable(ctx, confGame, c.gcid, confStake, 2, 10, GamingTableFunds{
		RefundBlocks: confRefund, AdmissionAtoms: confBond, AdmissionBlocks: confBondLock,
	})
	if err != nil {
		t.Fatalf("alice creates the table: %v", err)
	}
	c.sid, c.until = table.SID, int64(table.Until)
	if sid, err := c.bob.br.AcceptGamingInvite(ctx, confGame, table.Invite, c.gcid); err != nil || sid != c.sid {
		t.Fatalf("bob accepts = %q, %v", sid, err)
	}
	for _, p := range c.players() {
		confEventually(t, "a seat bond to wait on the operator", func() bool { return p.approvePending(t, ctx) > 0 })
	}
	c.waitFor(t, ctx, "both seats to bind themselves to the roster", c.until, func() bool {
		return c.phase(c.alice) == "settled" && c.phase(c.bob) == "settled"
	})

	// Alice's game seats first and hands her bridge the roster, so her
	// bridge's roster commitment reaches bob's before his seats are bound,
	// and is refused as early. Bob's bind has to apply it: nothing replays.
	beacon := int64(membership.BeaconHeight(c.alice.rt.Terms(c.sid)))
	for c.chain.height() < beacon {
		c.chain.mine(1)
	}
	confEventually(t, "bob's bridge to refuse alice's commitment as early", func() bool {
		c.alice.rt.Tick(ctx, c.chain.height())
		return c.bound(ctx, c.alice) && c.waiting(c.bob, c.alice)
	})
	c.waitFor(t, ctx, "both games to seat the same table", beacon, func() bool {
		_, a := c.alice.rt.Seats(c.sid)
		_, b := c.bob.rt.Seats(c.sid)
		return a && b
	})
	return c
}

// funded reports whether a game sees every seat's stake.
func (c *confTable) funded(p *confPlayer) bool {
	seats, ok := p.rt.Seats(c.sid)
	if !ok {
		return false
	}
	for seat := range seats {
		if _, _, ok := p.rt.Funded(c.sid, seat); !ok {
			return false
		}
	}
	return true
}

// awaitingApproval is the payout a bridge holds for its operator.
func (c *confTable) awaitingApproval(t *testing.T, ctx context.Context, p *confPlayer) string {
	t.Helper()
	var id string
	confEventually(t, "the payout to wait on the operator", func() bool {
		payouts, err := p.br.GamingPayouts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range payouts {
			if v.Table == c.sid && v.State == "awaiting_approval" {
				id = v.ID
				return true
			}
		}
		return false
	})
	return id
}

// A table's payout moves only once every seat's operator approved it, and
// then pays the winner's own wallet from every seat's stake.
func TestConformanceTwoBridgesPayOutATable(t *testing.T) {
	ctx := confContext(t)
	c := seatConfTable(t, ctx)
	// Funding waits for the operator, so each game funds while its operator
	// approves. A game asking before the two bridges have agreed the financial
	// roster is refused and asks again, as a game does.
	deadline := int64(membership.FundingDeadline(c.alice.rt.Terms(c.sid)))
	funding := make(chan error, 2)
	for _, p := range c.players() {
		go func() {
			for {
				err := p.rt.Fund(ctx, c.sid)
				if err == nil || !strings.Contains(err.Error(), "independent bridge verification") {
					funding <- err
					return
				}
				// At the pace the bridge takes money requests.
				select {
				case <-ctx.Done():
					funding <- ctx.Err()
					return
				case <-time.After(2 * time.Second):
				}
			}
		}()
	}
	answered := 0
	c.waitFor(t, ctx, "both stakes to be paid", deadline-1, func() bool {
		select {
		case err := <-funding:
			if err != nil {
				t.Fatalf("fund the stake: %v", err)
			}
			answered++
		default:
		}
		for _, p := range c.players() {
			p.approvePending(t, ctx)
		}
		return answered == 2
	})
	c.waitFor(t, ctx, "both games to see every stake", deadline-1, func() bool {
		return c.funded(c.alice) && c.funded(c.bob)
	})

	winner, _ := c.alice.rt.Seat(c.sid)
	outcome := runtime.Outcome{Shares: map[uint32]int64{winner: 2 * confStake}}
	// Alice's game proposes the payout and alice approves it before bob's
	// game has proposed it, so her signatures reach bob's bridge early. Bob's
	// proposal has to apply them: nothing replays.
	if err := c.alice.rt.Settle(ctx, c.sid, outcome); err != nil {
		t.Fatalf("alice's game proposes the payout: %v", err)
	}
	payoutID := c.awaitingApproval(t, ctx, c.alice)
	if _, err := c.alice.br.ApproveGamingPayout(ctx, payoutID, []byte(c.alice.host.wallet.pass)); err != nil {
		t.Fatalf("alice approves: %v", err)
	}
	confEventually(t, "bob's bridge to hold alice's early signatures", func() bool { return c.waiting(c.bob, c.alice) })
	payout, err := chainhash.NewHashFromStr(payoutID)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		c.chain.mine(1)
		c.tick(ctx)
		for _, p := range c.players() {
			if p.host.wallet.tried(*payout) {
				t.Fatal("a bridge broadcast the payout with one operator's approval")
			}
		}
	}

	if err := c.bob.rt.Settle(ctx, c.sid, outcome); err != nil {
		t.Fatalf("bob's game proposes the payout: %v", err)
	}
	if id := c.awaitingApproval(t, ctx, c.bob); id != payoutID {
		t.Fatalf("the bridges hold different payouts: %s and %s", payoutID, id)
	}
	if _, err := c.bob.br.ApproveGamingPayout(ctx, payoutID, []byte(c.bob.host.wallet.pass)); err != nil {
		t.Fatalf("bob approves: %v", err)
	}
	// While the payout waits in the mempool the node still reports the
	// stakes; each bridge says its own is being spent, and its game hears it.
	confEventually(t, "the payout to reach the mempool", func() bool {
		c.tick(ctx)
		tx, err := c.chain.GetRawTransactionVerbose(ctx, payout)
		return err == nil && tx.Confirmations == 0
	})
	c.tick(ctx)
	for i, p := range c.players() {
		name := []string{"alice", "bob"}[i]
		state, err := p.br.GamingFinancialState(ctx, confGame, c.sid)
		if err != nil {
			t.Fatal(err)
		}
		stakes := 0
		for _, d := range state.GetDeposits() {
			if d.GetKind() != "stake" {
				continue
			}
			stakes++
			if d.GetState() != "spend_pending" {
				t.Fatalf("%s's bridge reports its stake %s while the payout is in the mempool", name, d.GetState())
			}
		}
		if stakes != 1 {
			t.Fatalf("%s's bridge reports %d stakes", name, stakes)
		}
		snap, err := p.rt.RefreshDeposits(ctx, c.sid)
		if err != nil {
			t.Fatal(err)
		}
		seat, _ := p.rt.Seat(c.sid)
		own := 0
		for _, d := range snap.Deposits {
			if d.Purpose != "stake" || d.Seat != seat {
				continue
			}
			own++
			if d.Check != "spending" {
				t.Fatalf("%s's game reads its stake %s while the payout is in the mempool", name, d.Check)
			}
		}
		if own != 1 {
			t.Fatalf("%s's game reads %d stakes of its own", name, own)
		}
	}
	c.waitFor(t, ctx, "the payout to confirm on both bridges", c.chain.height()+10, func() bool {
		for _, p := range c.players() {
			st, err := p.br.GamingPayoutStatus(ctx, confGame, payoutID)
			if err != nil || st.GetState() != "confirmed" {
				return false
			}
		}
		return true
	})

	tx, err := c.chain.GetRawTransaction(ctx, payout)
	if err != nil {
		t.Fatal(err)
	}
	stakes := map[string]bool{}
	for seat := range 2 {
		op, _, ok := c.alice.rt.Funded(c.sid, uint32(seat))
		if !ok {
			t.Fatalf("seat %d has no stake", seat)
		}
		stakes[op] = true
	}
	for _, in := range tx.MsgTx().TxIn {
		delete(stakes, fmt.Sprintf("%s:%d", in.PreviousOutPoint.Hash, in.PreviousOutPoint.Index))
	}
	if len(stakes) != 0 {
		t.Fatalf("the payout left stakes unspent: %v", stakes)
	}
	var paid int64
	for _, out := range tx.MsgTx().TxOut {
		key, mine := c.alice.host.wallet.ownerLocked(out.PkScript, out.Version)
		if mine && key.account == fakeGamingAccount {
			paid += out.Value
		}
	}
	if paid <= 0 || paid > 2*confStake {
		t.Fatalf("alice's wallet was paid %d atoms of a %d atom pot", paid, 2*confStake)
	}
}
