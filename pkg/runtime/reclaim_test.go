package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/txscript/v4/stdaddr"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

const bondOutpoint = "aa11bb22aa11bb22aa11bb22aa11bb22aa11bb22aa11bb22aa11bb22aa11bb22:0"

// payTo is somewhere for a reclaim to send the coin.
func payTo(t *testing.T) string {
	t.Helper()
	var h [20]byte
	copy(h[:], "a twenty byte hash..")
	addr, err := stdaddrPKH(h[:])
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	return addr
}

// fundBond puts this seat's bond on the fake chain, matured, so a reclaim has
// something real to pull home.
func fundBond(t *testing.T, fake *bridgetest.Bridge, rt *Runtime, confirmations int64) {
	t.Helper()
	key, err := rt.identity.DeriveKey(rt.seatTags.Bond, "")
	if err != nil {
		t.Fatalf("bond key: %v", err)
	}
	script, err := escrow.BondScript(key.PubKey().SerializeCompressed(), escrow.MinBondBlocks)
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}
	_, pkScript, err := escrow.Address(script, chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("bond address: %v", err)
	}
	txid, vout, err := splitOutpoint(bondOutpoint)
	if err != nil {
		t.Fatalf("outpoint: %v", err)
	}
	fake.Place(txid, vout, pkScript, int64(escrow.MinBondAtoms), fake.Height()-confirmations+1)
}

func reclaimBond(rt *Runtime, dest string) (string, error) {
	return rt.doReclaim(context.Background(), &gamingpb.Reclaim{
		Kind: gamingpb.Reclaim_BOND, DestAddr: dest,
	})
}

// A matured bond comes home, and the identity stops citing a deposit that is on
// its way out.
func TestAMaturedBondComesHome(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	fundBond(t, fake, rt, int64(escrow.MinBondBlocks))

	txid, err := reclaimBond(rt, payTo(t))
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if txid == "" {
		t.Fatal("no transaction was broadcast")
	}
	if got := fake.SentCount(txid); got != 1 {
		t.Fatalf("broadcast %d times", got)
	}
	if rt.identity.BondDeposit() != "" {
		t.Fatal("the identity still cites a bond it has spent")
	}
}

// The guard that matters most: dcrd cannot see a mempool spend, so this process
// asking twice would be a double spend.
func TestASecondReclaimOfTheSameOutputIsRefused(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	fundBond(t, fake, rt, int64(escrow.MinBondBlocks))
	dest := payTo(t)

	if _, err := reclaimBond(rt, dest); err != nil {
		t.Fatalf("first reclaim: %v", err)
	}
	// Put the deposit back, so only the sweeping guard can refuse it.
	if err := rt.identity.SetBondDeposit(bondOutpoint); err != nil {
		t.Fatalf("restore: %v", err)
	}
	_, err := reclaimBond(rt, dest)
	if err == nil {
		t.Fatal("spent the same output twice")
	}
	if !strings.Contains(err.Error(), "double spend") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	if n := len(fake.Spends()); n != 0 {
		t.Fatalf("a reclaim asked the bridge for a spend: %d", n)
	}
}

// A lock that has not matured is refused, and the refusal says how long is left
// rather than only that it failed.
func TestAnImmatureBondIsRefusedWithTheBlocksLeft(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	fundBond(t, fake, rt, int64(escrow.MinBondBlocks)-10)

	_, err := reclaimBond(rt, payTo(t))
	if err == nil {
		t.Fatal("spent a bond before its lock matured")
	}
	if !strings.Contains(err.Error(), "another 10 blocks") {
		t.Fatalf("the refusal does not say how long is left: %v", err)
	}
}

// An output that is not there is not a reclaim.
func TestReclaimingAnOutputThatHoldsNoCoinIsRefused(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	if _, err := reclaimBond(rt, payTo(t)); err == nil {
		t.Fatal("reclaimed an output the chain does not have")
	}
}

// The script has to be the one the output was paid into. BuildTimelockedSpend
// cannot notice being handed the wrong script, so this check is the only thing
// between a wrong key and a signature dcrd rejects much later.
func TestAScriptTheOutputWasNotPaidIntoIsRefusedBeforeSigning(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	txid, vout, err := splitOutpoint(bondOutpoint)
	if err != nil {
		t.Fatalf("outpoint: %v", err)
	}
	// Somebody else's script at this seat's bond outpoint.
	fake.Place(txid, vout, []byte{0x76, 0xa9, 0x14, 0x01, 0x02},
		int64(escrow.MinBondAtoms), fake.Height()-int64(escrow.MinBondBlocks)+1)

	_, err = reclaimBond(rt, payTo(t))
	if err == nil {
		t.Fatal("signed a spend of an output this key does not open")
	}
	if !strings.Contains(err.Error(), "the coin is untouched") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

func TestAReclaimMustSayWhereToSendIt(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	_, err := rt.doReclaim(context.Background(), &gamingpb.Reclaim{Kind: gamingpb.Reclaim_BOND})
	if err == nil {
		t.Fatal("reclaimed to nowhere")
	}
}

func TestAReclaimToSomethingThatIsNotAnAddressIsRefused(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	fundBond(t, fake, rt, int64(escrow.MinBondBlocks))
	if _, err := reclaimBond(rt, "not-an-address"); err == nil {
		t.Fatal("built a payment to something that is not an address")
	}
}

// The two kinds that need a table's deposit records say so plainly rather than
// guessing at a script.
func TestTheKindsThatNeedFundingSaySo(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	for _, kind := range []gamingpb.Reclaim_Kind{
		gamingpb.Reclaim_STAKE, gamingpb.Reclaim_TABLE_BOND,
	} {
		_, err := rt.doReclaim(context.Background(), &gamingpb.Reclaim{
			Kind: kind, Sid: "abcdef01", DestAddr: payTo(t),
		})
		if !errors.Is(err, ErrNotYet) {
			t.Errorf("%v: %v", kind, err)
		}
	}
}

// The bond key is derived without a session id, because the deposit it opens is
// one per identity. Deriving it per table would build a script the stored
// deposit was never paid into.
func TestTheBondKeyDoesNotDependOnTheTable(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	a, err := rt.identity.DeriveKey(rt.seatTags.Bond, "")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	perTable, err := rt.identity.DeriveKey(rt.seatTags.Bond, "abcdef01")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if a.PubKey().IsEqual(perTable.PubKey()) {
		t.Fatal("the session id does not change the derived key, so this test proves nothing")
	}

	// What a join binds to must be what a reclaim opens.
	creds, err := rt.seatCredentials(mustTerms(t, rt, "abcdef01"))
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if !creds.Bond.PubKey().IsEqual(a.PubKey()) {
		t.Fatal("a join binds to a bond key a reclaim cannot open")
	}
}

func TestSplitOutpointRefusesWhatIsNotOne(t *testing.T) {
	for _, bad := range []string{
		"", "abc", "abc:", ":0", "abc:x",
		strings.Repeat("a", 63) + ":0",
		strings.Repeat("a", 65) + ":0",
	} {
		if _, _, err := splitOutpoint(bad); err == nil {
			t.Errorf("%q was read as an outpoint", bad)
		}
	}
	txid, vout, err := splitOutpoint(bondOutpoint)
	if err != nil || vout != 0 || len(txid) != 64 {
		t.Fatalf("a real outpoint did not parse: %q %d %v", txid, vout, err)
	}
}

func TestSweepingIsRememberedAndForgotten(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	if rt.isSweeping("x:0") {
		t.Fatal("an outpoint nobody swept reads as sweeping")
	}
	rt.noteSweeping("x:0")
	if !rt.isSweeping("x:0") {
		t.Fatal("a broadcast spend was not remembered")
	}
	rt.doneSweeping("x:0")
	if rt.isSweeping("x:0") {
		t.Fatal("an outpoint the chain took away is still remembered")
	}
}

// helpers

func stdaddrPKH(h []byte) (string, error) {
	a, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(h, chaincfg.TestNet3Params())
	if err != nil {
		return "", err
	}
	return a.String(), nil
}

func mustTerms(t *testing.T, rt *Runtime, sid string) membership.Terms {
	t.Helper()
	got, err := rt.rules.Terms(sid)
	if err != nil {
		t.Fatalf("terms: %v", err)
	}
	got.SID = sid
	return got
}
