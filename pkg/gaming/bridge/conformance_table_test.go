// Copyright (c) 2015-2026 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package bridge

import (
	"context"
	"fmt"
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
// bridges' part being what their financial worker does every thirty seconds.
func (c *confTable) tick(ctx context.Context) {
	for _, p := range c.players() {
		for _, ev := range p.br.financialReplay() {
			p.br.processFinancialFrame(ctx, ev)
		}
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
	c.waitFor(t, ctx, "both seats to join", c.until, func() bool {
		for _, p := range c.players() {
			if ph := c.phase(p); ph == "" || ph == "admission" || ph == "joining" {
				return false
			}
		}
		return true
	})
	beacon := int64(membership.BeaconHeight(c.alice.rt.Terms(c.sid)))
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
				select {
				case <-ctx.Done():
					funding <- ctx.Err()
					return
				case <-time.After(50 * time.Millisecond):
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
	for _, p := range c.players() {
		if err := p.rt.Settle(ctx, c.sid, outcome); err != nil {
			t.Fatalf("propose the payout: %v", err)
		}
	}
	aliceID := c.awaitingApproval(t, ctx, c.alice)
	bobID := c.awaitingApproval(t, ctx, c.bob)
	if aliceID != bobID {
		t.Fatalf("the bridges hold different payouts: %s and %s", aliceID, bobID)
	}

	if _, err := c.alice.br.ApproveGamingPayout(ctx, aliceID, []byte(c.alice.host.wallet.pass)); err != nil {
		t.Fatalf("alice approves: %v", err)
	}
	payout, err := chainhash.NewHashFromStr(aliceID)
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

	if _, err := c.bob.br.ApproveGamingPayout(ctx, bobID, []byte(c.bob.host.wallet.pass)); err != nil {
		t.Fatalf("bob approves: %v", err)
	}
	c.waitFor(t, ctx, "the payout to confirm on both bridges", c.chain.height()+10, func() bool {
		for _, p := range c.players() {
			st, err := p.br.GamingPayoutStatus(ctx, confGame, aliceID)
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
