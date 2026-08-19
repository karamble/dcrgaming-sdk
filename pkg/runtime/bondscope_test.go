package runtime

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/connect"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
	"github.com/karamble/dcrgaming-sdk/pkg/identity"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

// perTableGame posts a fresh bond for every table, the way dcrbattleships
// does.
type perTableGame struct{ battleshipsRules }

// standPerTable brings up a runtime that posts one bond per table. It is given
// no identity-wide deposit at all, which is the point: a game that bonds per
// table must not need one.
func standPerTable(t *testing.T) (*bridgetest.Bridge, *Runtime) {
	t.Helper()
	g := &perTableGame{}
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

	conn, err := srv.Dial(ctx, "seat0", func(cfg *transport.BridgeConfig) {
		connect.Stamp(cfg, g.Identity())
	})
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
	rt, err := New(Config{
		Rules: g, Bridge: conn, Book: book, Tables: NewMemTableStore(),
		Identity: seed, SeatTags: testTags, Params: chaincfg.TestNet3Params(),
		PunishTag: []byte("testgame/punishkey/v1"),
		BondScope: BondPerTable,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	go func() { _ = rt.Run(ctx) }()
	return fake, rt
}

// A game that bonds per table joins without an identity-wide deposit.
//
// This is the whole of the difference, and it is an economic choice rather
// than a protocol one: one bond backing every table is cheaper for a player at
// several, and a bond per table cannot stand behind two seats at once. The SDK
// must be able to say either, or it is not game-agnostic - it is one game with
// the other's assumptions baked in.
func TestAGameThatBondsPerTableNeedsNoIdentityDeposit(t *testing.T) {
	fake, rt := standPerTable(t)
	ctx := context.Background()

	if got := rt.identity.BondDeposit(); got != "" {
		t.Fatalf("this seat holds an identity deposit (%q), so this proves nothing", got)
	}
	sid, err := rt.AcceptInvite(ctx, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	// Accepted, and not yet joined: the bond it will join with is still
	// being paid for.
	waitFor(t, "the table to join once its bond lands", func() bool {
		_, err := rt.tableOf(sid)
		return err == nil
	})

	tbl, err := rt.tableOf(sid)
	if err != nil {
		t.Fatalf("table: %v", err)
	}
	// The join binds to the bond this table paid, not to an identity's.
	ours := tbl.form.Ours()
	if ours == nil {
		t.Fatal("this seat has no join")
	}
	rt.mu.Lock()
	paid := tbl.seatBond.outpoint
	rt.mu.Unlock()
	if paid == "" {
		t.Fatal("no seat bond was paid")
	}
	if ours.Bond.Outpoint != paid {
		t.Fatalf("the join binds to %q and this table paid %q", ours.Bond.Outpoint, paid)
	}
	// And it really is on the fake chain, at the script the terms describe.
	txid, vout, err := splitOutpoint(paid)
	if err != nil {
		t.Fatalf("outpoint: %v", err)
	}
	out, ok := fake.Output(txid, vout)
	if !ok {
		t.Fatalf("%s holds nothing", paid)
	}
	redeem, err := rt.seatBondScript(tbl.terms)
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}
	want, _, err := membership.PkScriptAndAddr(redeem, chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("bond address: %v", err)
	}
	if !strings.EqualFold(hex.EncodeToString(out.PkScript), want) {
		t.Fatalf("the seat bond was paid into %x and this table derives %s",
			out.PkScript, want)
	}
}

// The two scopes derive different keys, and each opens what it should.
//
// A key derived the wrong way builds a script the coin was never paid into, so
// the bond is real, unspendable, and bound to a join nobody can honour.
func TestEachBondScopeDerivesItsOwnKey(t *testing.T) {
	_, perTable := standPerTable(t)
	_, perIdentity, _ := stand(t, &trivialGame{})

	sid := "abcdef01"
	a, err := perTable.seatBondKeyFor(sid)
	if err != nil {
		t.Fatalf("per table: %v", err)
	}
	withoutSID, err := perTable.identity.DeriveKey(perTable.seatTags.Bond, "")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if a.PubKey().IsEqual(withoutSID.PubKey()) {
		t.Fatal("a per-table bond key does not depend on the table")
	}

	b, err := perIdentity.seatBondKeyFor(sid)
	if err != nil {
		t.Fatalf("per identity: %v", err)
	}
	plain, err := perIdentity.identity.DeriveKey(perIdentity.seatTags.Bond, "")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if !b.PubKey().IsEqual(plain.PubKey()) {
		t.Fatal("a per-identity bond key depends on the table, so it opens nothing")
	}
}

// A game that bonds per identity is not asked to fund a table's bond, because
// there is no such thing to fund.
func TestFundingATableBondIsRefusedWhereBondsArePerIdentity(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	err = rt.FundSeatBond(context.Background(), sid)
	if err == nil {
		t.Fatal("a per-identity game funded a per-table bond")
	}
	if !strings.Contains(err.Error(), "one bond per identity") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// A table that has not joined yet is still a table, and a restart finds it.
func TestATableStillPayingForItsSeatSurvivesARestart(t *testing.T) {
	_, rt := standPerTable(t)
	sid, err := rt.AcceptInvite(context.Background(), invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	waitFor(t, "the table to join", func() bool {
		_, err := rt.tableOf(sid)
		return err == nil
	})
	tbl, _ := rt.tableOf(sid)
	rt.mu.Lock()
	paid := tbl.seatBond.outpoint
	rt.mu.Unlock()

	recs, err := rt.store.LoadTables()
	if err != nil || len(recs) != 1 {
		t.Fatalf("load: %v %d", err, len(recs))
	}
	if recs[0].SeatBond.Outpoint != paid {
		t.Fatalf("the record says the seat bond is at %q, and it is at %q",
			recs[0].SeatBond.Outpoint, paid)
	}
	if recs[0].SeatBond.Atoms != int64(escrow.MinBondAtoms) &&
		recs[0].SeatBond.Atoms != int64(tbl.terms.BondAtoms) {
		t.Fatalf("the seat bond is recorded as %d atoms", recs[0].SeatBond.Atoms)
	}
	if _, ok := tbl.form.Seats(); ok {
		t.Fatal("a table with one join reports seating")
	}
}
