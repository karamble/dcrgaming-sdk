package runtime

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

// theirFunding is the other seat saying where its stake is, the way it arrives
// over the wire.
func theirFunding(t *testing.T, them *peer, outpoint string) schema.Funded {
	t.Helper()
	seat, ok := them.form.OurSeat()
	if !ok {
		t.Fatal("the other side has no seat")
	}
	f, err := membership.SignFunding(them.form.Terms(), seat, outpoint, them.creds.Session)
	if err != nil {
		t.Fatalf("their funding: %v", err)
	}
	return schema.FundedFrom(f)
}

// placeTheirStake puts the other seat's stake on the fake chain, in the escrow
// this table derived for it, and returns the outpoint.
func placeTheirStake(t *testing.T, fake *bridgetest.Bridge, rt *Runtime, sid string, them *peer) string {
	t.Helper()
	seat, _ := them.form.OurSeat()
	tbl, err := rt.tableOf(sid)
	if err != nil {
		t.Fatalf("table: %v", err)
	}
	dep, err := rt.depositFor(tbl, seat)
	if err != nil {
		t.Fatalf("their deposit: %v", err)
	}
	script, err := hex.DecodeString(dep.pkScript)
	if err != nil {
		t.Fatalf("pkScript: %v", err)
	}
	txid := stakeTxid(seat)
	fake.Place(txid, 0, script, stakeAtoms, fake.Height())
	return txid + ":0"
}

// A seat pays its own stake and is the only one that saw it happen. Without
// this the other side never learns, and a settlement built over one stake is
// one nobody else will sign.
func TestASeatLearnsWhereAnotherSeatsStakeIs(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	theirSeat, _ := them.form.OurSeat()

	if _, _, ok := rt.Funded(sid, theirSeat); ok {
		t.Fatal("their stake was known before they said anything")
	}
	outpoint := placeTheirStake(t, fake, rt, sid, them)
	if err := rt.adoptFunded(context.Background(), sid, theirFunding(t, them, outpoint)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	got, atoms, ok := rt.Funded(sid, theirSeat)
	if !ok || got != outpoint {
		t.Fatalf("their stake is recorded as %q/%v", got, ok)
	}
	if atoms != stakeAtoms {
		t.Fatalf("their stake is recorded as %d atoms, not %d", atoms, stakeAtoms)
	}
}

// A signature says who spoke, not what is on the chain. Without checking the
// output a seat could point the settlement at one it can spend alone, and
// every other seat would sign a transaction handing it the pot.
func TestAStakeMustPayTheScriptThisTableDerived(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)

	// An output the announcer controls, paying an ordinary address.
	pay, err := payScriptFor(payTo(t), chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("pay script: %v", err)
	}
	mine := strings.Repeat("cd", 32)
	fake.Place(mine, 0, pay, stakeAtoms, fake.Height())

	err = rt.adoptFunded(context.Background(), sid, theirFunding(t, them, mine+":0"))
	if err == nil {
		t.Fatal("a stake was accepted at an output the announcer controls")
	}
	if !strings.Contains(err.Error(), "does not pay the script") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	theirSeat, _ := them.form.OurSeat()
	if _, _, ok := rt.Funded(sid, theirSeat); ok {
		t.Fatal("it was recorded anyway")
	}
}

// An outpoint that holds nothing is not a stake.
func TestAStakeAtAnOutpointThatHoldsNothingIsRefused(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	err := rt.adoptFunded(context.Background(), sid,
		theirFunding(t, them, strings.Repeat("ef", 32)+":0"))
	if err == nil {
		t.Fatal("a stake was accepted at an outpoint holding nothing")
	}
	if !strings.Contains(err.Error(), "holds no coin") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// A seat that staked less than the buy-in would be playing for a pot everyone
// else filled.
func TestAStakeShortOfTheBuyInIsRefused(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	seat, _ := them.form.OurSeat()
	tbl, _ := rt.tableOf(sid)
	dep, err := rt.depositFor(tbl, seat)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	script, _ := hex.DecodeString(dep.pkScript)
	short := strings.Repeat("12", 32)
	fake.Place(short, 0, script, int64(tbl.formation().Terms().BuyInAtoms)-1, fake.Height())

	err = rt.adoptFunded(context.Background(), sid, theirFunding(t, them, short+":0"))
	if err == nil {
		t.Fatal("a stake short of the buy-in was accepted")
	}
	if !strings.Contains(err.Error(), "this table costs") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// An announcement is signed by the seat it names, so nobody can place somebody
// else's stake and nobody at all can place one for a table they are not at.
func TestAnAnnouncementMustComeFromTheSeatItNames(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	outpoint := placeTheirStake(t, fake, rt, sid, them)
	good := theirFunding(t, them, outpoint)

	for _, tc := range []struct {
		name string
		mut  func(*schema.Funded)
		want string
	}{
		{"a seat that is not at this table", func(f *schema.Funded) { f.Seat = 9 }, "not at this table"},
		{"another seat's number", func(f *schema.Funded) { f.Seat = 1 - f.Seat }, "signed by somebody else"},
		{"a forged signature", func(f *schema.Funded) {
			raw, _ := hex.DecodeString(f.Sig)
			raw[0] ^= 0xff
			f.Sig = hex.EncodeToString(raw)
		}, "stake"},
	} {
		bad := good
		tc.mut(&bad)
		err := rt.adoptFunded(context.Background(), sid, bad)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: refused for the wrong reason: %v", tc.name, err)
		}
	}
}

// A seat cannot move its stake once it has said where it is: a settlement may
// already have been built over the first one.
func TestASeatCannotMoveItsStakeOnceAnnounced(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	seat, _ := them.form.OurSeat()
	first := placeTheirStake(t, fake, rt, sid, them)
	if err := rt.adoptFunded(context.Background(), sid, theirFunding(t, them, first)); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Saying the same thing again is a repeat, not a change.
	if err := rt.adoptFunded(context.Background(), sid, theirFunding(t, them, first)); err != nil {
		t.Fatalf("a repeated announcement was refused: %v", err)
	}

	tbl, _ := rt.tableOf(sid)
	dep, _ := rt.depositFor(tbl, seat)
	script, _ := hex.DecodeString(dep.pkScript)
	second := strings.Repeat("34", 32)
	fake.Place(second, 0, script, stakeAtoms, fake.Height())

	err := rt.adoptFunded(context.Background(), sid, theirFunding(t, them, second+":0"))
	if err == nil {
		t.Fatal("a seat moved its stake after announcing it")
	}
	if !strings.Contains(err.Error(), "already placed its stake") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	if got, _, _ := rt.Funded(sid, seat); got != first {
		t.Fatalf("the stake moved to %q", got)
	}
}

// A payout accepted from anybody would let a bystander redirect the pot.
func TestAPayoutIsRefusedFromSomebodyElse(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	seat, _ := them.form.OurSeat()

	p, err := membership.SignPayout(them.form.Terms(), seat, payTo(t), them.creds.Session)
	if err != nil {
		t.Fatalf("their payout: %v", err)
	}
	good := schema.PayoutFrom(p)
	if err := rt.adoptPayout(context.Background(), sid, good); err != nil {
		t.Fatalf("a real payout was refused: %v", err)
	}

	bad := good
	bad.Seat = 1 - seat // the same signature, claiming the other seat
	if err := rt.adoptPayout(context.Background(), sid, bad); err == nil {
		t.Fatal("a payout was accepted for a seat that did not sign it")
	}
}

// An address rather than a script on the wire, so one for the wrong chain is
// refused here instead of at settlement.
func TestAPayoutForTheWrongChainIsRefused(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	seat, _ := them.form.OurSeat()

	p, err := membership.SignPayout(them.form.Terms(), seat, "not-an-address", them.creds.Session)
	if err != nil {
		t.Fatalf("their payout: %v", err)
	}
	if err := rt.adoptPayout(context.Background(), sid, schema.PayoutFrom(p)); err == nil {
		t.Fatal("a payout to something that is not an address was accepted")
	}
}

// The first announcement goes out seconds after the broadcast, when every peer
// still refuses it for want of a confirmation. Said once, that is a message
// guaranteed to be refused and never repeated.
func TestTheStakeIsSaidAgainUntilTheTableIsFunded(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	ctx := context.Background()

	// This seat's stake is on the chain; the other's is not yet known.
	tbl, _ := rt.tableOf(sid)
	mine, _ := tbl.formation().OurSeat()
	dep, _ := rt.depositFor(tbl, mine)
	script, _ := hex.DecodeString(dep.pkScript)
	ours := stakeTxid(mine)
	fake.Place(ours, 0, script, stakeAtoms, fake.Height())
	rt.mu.Lock()
	tbl.funded = map[uint32]staked{mine: {outpoint: ours + ":0", atoms: stakeAtoms}}
	// This test counts stake announcements. Mark payout destinations known so
	// their independent retry loop does not affect that count.
	tbl.payouts = map[uint32][]byte{0: {0x51}, 1: {0x51}}
	rt.mu.Unlock()

	at := fake.Height()
	before := len(fake.Sent())
	rt.Tick(ctx, at+1)
	first := len(fake.Sent())
	if first <= before {
		t.Fatal("the stake was not said again while the table was short")
	}
	// Once a block, not once a poll.
	rt.Tick(ctx, at+1)
	if len(fake.Sent()) != first {
		t.Fatalf("it was said twice at one height: %d then %d", first, len(fake.Sent()))
	}
	// A new block says it again.
	rt.Tick(ctx, at+2)
	second := len(fake.Sent())
	if second <= first {
		t.Fatal("it was not said again at the next block")
	}

	// Once everybody's stake is known, there is nobody left to tell.
	outpoint := placeTheirStake(t, fake, rt, sid, them)
	if err := rt.adoptFunded(ctx, sid, theirFunding(t, them, outpoint)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	quiet := len(fake.Sent())
	rt.Tick(ctx, at+3)
	if len(fake.Sent()) != quiet {
		t.Fatal("the stake was still being announced after the table was funded")
	}
}

// A tick at no height is not a block, and must not be treated as one.
func TestATickAtNoHeightDoesNothing(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := seatTwo(t, fake, rt)

	// With a stake to announce and a seat still short of it, so there is
	// something a tick would send if it treated this as a block.
	tbl, _ := rt.tableOf(sid)
	mine, _ := tbl.formation().OurSeat()
	dep, _ := rt.depositFor(tbl, mine)
	script, _ := hex.DecodeString(dep.pkScript)
	ours := stakeTxid(mine)
	fake.Place(ours, 0, script, stakeAtoms, fake.Height())
	rt.mu.Lock()
	tbl.funded = map[uint32]staked{mine: {outpoint: ours + ":0", atoms: stakeAtoms}}
	rt.mu.Unlock()

	// A block first, so there is a marker to drag backwards.
	rt.Tick(context.Background(), fake.Height()+1)
	rt.mu.Lock()
	marker := tbl.saidAt
	rt.mu.Unlock()
	if marker == 0 {
		t.Fatal("the block was not marked, so this proves nothing")
	}

	before := len(fake.Sent())
	rt.Tick(context.Background(), 0)
	if len(fake.Sent()) != before {
		t.Fatal("a tick at height zero sent something")
	}
	rt.mu.Lock()
	after := tbl.saidAt
	rt.mu.Unlock()
	if after != marker {
		t.Fatalf("a tick at no height moved the block marker from %d to %d; "+
			"the next real block would say everything again", marker, after)
	}
}

// A seat cannot change where it is paid once it has said: a settlement may
// already have been signed over the first answer.
func TestASeatCannotChangeItsPayoutOnceAnnounced(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	seat, _ := them.form.OurSeat()

	first, err := membership.SignPayout(them.form.Terms(), seat, payTo(t), them.creds.Session)
	if err != nil {
		t.Fatalf("their payout: %v", err)
	}
	if err := rt.adoptPayout(context.Background(), sid, schema.PayoutFrom(first)); err != nil {
		t.Fatalf("first: %v", err)
	}
	// The same one again is a repeat, not a change.
	if err := rt.adoptPayout(context.Background(), sid, schema.PayoutFrom(first)); err != nil {
		t.Fatalf("a repeated payout was refused: %v", err)
	}

	elsewhere := elsewherePay(t)
	if elsewhere == first.Address {
		t.Fatal("both payouts are the same address, so this proves nothing")
	}
	second, err := membership.SignPayout(them.form.Terms(), seat, elsewhere, them.creds.Session)
	if err != nil {
		t.Fatalf("their second payout: %v", err)
	}
	err = rt.adoptPayout(context.Background(), sid, schema.PayoutFrom(second))
	if err == nil {
		t.Fatal("a seat redirected its payout after announcing it")
	}
	if !strings.Contains(err.Error(), "already said where to be paid") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	want, err := payScriptFor(first.Address, chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("pay script: %v", err)
	}
	tbl, _ := rt.tableOf(sid)
	rt.mu.Lock()
	got := tbl.payouts[seat]
	rt.mu.Unlock()
	if !bytesEqual(got, want) {
		t.Fatal("the payout moved anyway")
	}
}

// elsewherePay is a second address, somewhere other than payTo.
func elsewherePay(t *testing.T) string {
	t.Helper()
	var h [20]byte
	copy(h[:], "another twenty bytes")
	addr, err := stdaddrPKH(h[:])
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	return addr
}

// A forged signature naming the right key is refused.
//
// The seat check alone does not do it: the signer field travels with the
// message, so anybody can set it to the seat's real key. Only the signature
// says the seat actually spoke.
func TestAForgedPayoutSignatureIsRefused(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	seat, _ := them.form.OurSeat()
	terms := them.form.Terms()
	ctx := context.Background()

	bend := func(hexed string) string {
		raw, err := hex.DecodeString(hexed)
		if err != nil || len(raw) == 0 {
			t.Fatalf("signature %q: %v", hexed, err)
		}
		raw[0] ^= 0xff
		return hex.EncodeToString(raw)
	}

	pay, err := membership.SignPayout(terms, seat, payTo(t), them.creds.Session)
	if err != nil {
		t.Fatalf("their payout: %v", err)
	}
	forgedPay := schema.PayoutFrom(pay)
	forgedPay.Sig = bend(forgedPay.Sig)
	if err := rt.adoptPayout(ctx, sid, forgedPay); err == nil {
		t.Error("a payout with a forged signature was accepted")
	}
}
