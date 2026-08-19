package runtime

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/txscript/v4/stdaddr"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/identity"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

// peer is the other seat: a real identity with real keys, forming the same
// table from its own side.
//
// Two Formations rather than one with a fabricated join, because a settlement
// needs two seats' signatures and a fabricated seat has no key to sign with.
// This is the protocol as it actually runs.
type peer struct {
	seed  *identity.Identity
	form  *membership.Formation
	creds membership.Credentials
	tags  identity.SeatTags
}

func newPeer(t *testing.T, terms membership.Terms) *peer {
	t.Helper()
	seed, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatalf("their identity: %v", err)
	}
	tags := identity.SeatTags{
		Session: "othergame/table-session/v1",
		Log:     "othergame/table-log/v1",
		Bond:    "othergame/bond/v1",
	}
	session, err := seed.DeriveKey(tags.Session, terms.SID)
	if err != nil {
		t.Fatalf("their session key: %v", err)
	}
	logKey, err := seed.DeriveKey(tags.Log, terms.SID)
	if err != nil {
		t.Fatalf("their log key: %v", err)
	}
	bond, err := seed.DeriveKey(tags.Bond, "")
	if err != nil {
		t.Fatalf("their bond key: %v", err)
	}
	lock := terms.BondLockBlocks
	if lock == 0 {
		lock = escrow.MinBondBlocks
	}
	script, err := escrow.BondScript(bond.PubKey().SerializeCompressed(), lock)
	if err != nil {
		t.Fatalf("their bond script: %v", err)
	}
	creds := membership.Credentials{
		Session: session, Log: logKey, Bond: bond,
		BondOutpoint: "bb22cc33bb22cc33bb22cc33bb22cc33bb22cc33bb22cc33bb22cc33bb22cc33:1",
		BondScript:   script,
	}
	form, err := membership.NewFormation(terms, creds)
	if err != nil {
		t.Fatalf("their formation: %v", err)
	}
	return &peer{seed: seed, form: form, creds: creds, tags: tags}
}

// seatTwo forms and seats a real two-seat table on both sides.
//
// Returns the session id and the other seat, which holds the keys a settlement
// needs the second signature from.
func seatTwo(t *testing.T, fake *bridgetest.Bridge, rt *Runtime) (string, *peer) {
	t.Helper()
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	rt.mu.Lock()
	tbl := rt.tables[sid]
	rt.mu.Unlock()

	them := newPeer(t, tbl.form.Terms())
	ours, theirs := tbl.form.Ours(), them.form.Ours()
	if ours == nil || theirs == nil {
		t.Fatal("a seat has no join of its own")
	}
	if err := tbl.form.AddJoin(theirs); err != nil {
		t.Fatalf("our side taking their join: %v", err)
	}
	if err := them.form.AddJoin(ours); err != nil {
		t.Fatalf("their side taking our join: %v", err)
	}
	tbl.form.CloseWindow()
	them.form.CloseWindow()

	ourA, err := tbl.form.Assertion()
	if err != nil {
		t.Fatalf("our assertion: %v", err)
	}
	theirA, err := them.form.Assertion()
	if err != nil {
		t.Fatalf("their assertion: %v", err)
	}
	both := []*membership.Join{ours, theirs}
	if err := tbl.form.AddAssertion(theirA, both); err != nil {
		t.Fatalf("our side taking their assertion: %v", err)
	}
	if err := them.form.AddAssertion(ourA, both); err != nil {
		t.Fatalf("their side taking our assertion: %v", err)
	}
	if !tbl.form.Agreed() || !them.form.Agreed() {
		t.Fatalf("the two sides did not agree: ours=%v theirs=%v",
			tbl.form.Agreed(), them.form.Agreed())
	}

	// The beacon is a block hash, so both sides read the same one.
	want := tbl.form.BeaconHeight()
	fake.SetHeight(int64(want) + 1)
	if err := rt.seatIfReady(context.Background(), sid); err != nil {
		t.Fatalf("seat: %v", err)
	}
	raw, err := hex.DecodeString(bridgetest.BlockHashHex(want))
	if err != nil {
		t.Fatalf("beacon: %v", err)
	}
	if err := them.form.SetBeacon(raw); err != nil {
		t.Fatalf("their beacon: %v", err)
	}
	if _, ok := tbl.form.Seats(); !ok {
		t.Fatal("our side did not seat")
	}
	if _, ok := them.form.Seats(); !ok {
		t.Fatal("their side did not seat")
	}
	return sid, them
}

// fundBoth puts both seats' stakes on the fake chain and records where.
func fundBoth(t *testing.T, fake *bridgetest.Bridge, rt *Runtime, sid string, them *peer) {
	t.Helper()
	rt.mu.Lock()
	tbl := rt.tables[sid]
	rt.mu.Unlock()

	deposits, err := tbl.form.Deposits(chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("deposits: %v", err)
	}
	pay, err := stdaddr.DecodeAddress(payTo(t), chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("payout: %v", err)
	}
	_, payScript := pay.PaymentScript()

	rt.mu.Lock()
	tbl.funded = map[uint32]staked{}
	tbl.payouts = map[uint32][]byte{}
	rt.mu.Unlock()

	for _, d := range deposits {
		txid := stakeTxid(d.Seat)
		script, err := hex.DecodeString(d.PkScriptHex)
		if err != nil {
			t.Fatalf("seat %d pkScript: %v", d.Seat, err)
		}
		fake.Place(txid, 0, script, stakeAtoms, fake.Height())
		rt.mu.Lock()
		tbl.funded[d.Seat] = staked{outpoint: txid + ":0", atoms: stakeAtoms}
		tbl.payouts[d.Seat] = payScript
		rt.mu.Unlock()
	}
}

func stakeTxid(seat uint32) string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(0xc0 + seat)
	}
	return hex.EncodeToString(b)
}

// The harness itself has to work, or every test built on it proves nothing.
func TestTwoSeatsReallyForm(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)

	seats, ok := rt.Seats(sid)
	if !ok || len(seats) != 2 {
		t.Fatalf("seats are %v", seats)
	}
	theirs, ok := them.form.Seats()
	if !ok || len(theirs) != 2 {
		t.Fatalf("their seats are %v", theirs)
	}
	// Both sides must agree who sits where, or nothing signed by one
	// verifies for the other.
	for seat, key := range seats {
		if hex.EncodeToString(theirs[seat]) != hex.EncodeToString(key) {
			t.Fatalf("seat %d is a different player on each side", seat)
		}
	}
}
