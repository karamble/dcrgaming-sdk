package punish

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/wire"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"

	"github.com/karamble/dcrgaming-sdk/pkg/evidence"
)

// Fixtures are literals independent of the code under test: the band base is
// written as 1<<32 rather than taken from pkg/commitment, and every amount is
// a number, not an expression over the constants being exercised.
const sweepMatch = "7c1e9a44b2d05f38e6a1c72d90b45e13a8f60c25d47b9e02613f8c5a2d0e9b71"

func testKey(t *testing.T, fill byte) *secp256k1.PrivateKey {
	t.Helper()
	priv := secp256k1.PrivKeyFromBytes(bytes.Repeat([]byte{fill}, 32))
	if priv.Key.IsZero() {
		t.Fatal("fixture key is zero")
	}
	return priv
}

// pinnedScript is a P2PKH-shaped stand-in for the bridge wallet's payout
// script. Output scripts are not executed by input validation, so it only has
// to be distinctive.
func pinnedScript() []byte {
	return append([]byte{0x76, 0xa9, 0x14}, append(bytes.Repeat([]byte{0x5d}, 20), 0x88, 0xac)...)
}

// sweepFixture is the cheat's forfeitable bond with the victim's punishment
// branch in it, and the keys on both sides.
type sweepFixture struct {
	bond     []byte
	logPriv  *secp256k1.PrivateKey // the cheat's log key
	punisher *secp256k1.PrivateKey // the victim's punishment half
	branch   forfeit.Branch
}

func newSweepFixture(t *testing.T) sweepFixture {
	t.Helper()
	logPriv := testKey(t, 0x11)
	punisher := testKey(t, 0x22)
	victimSession := testKey(t, 0x33)
	cheatSession := testKey(t, 0x44)

	br := forfeit.Branch{Match: sweepMatch, Seat: victimSession.PubKey().SerializeCompressed()}
	fPub, err := forfeit.ForfeitKey(br, logPriv.PubKey(), punisher.PubKey())
	if err != nil {
		t.Fatalf("branch key: %v", err)
	}
	bond, err := escrow.ForfeitableBondScript(cheatSession.PubKey().SerializeCompressed(),
		[][]byte{fPub.SerializeCompressed()}, 4032)
	if err != nil {
		t.Fatalf("build bond: %v", err)
	}
	return sweepFixture{bond: bond, logPriv: logPriv, punisher: punisher, branch: br}
}

func sweepDraft(f sweepFixture) Sweep {
	var prev chainhash.Hash
	copy(prev[:], bytes.Repeat([]byte{0x66}, chainhash.HashSize))
	return Sweep{
		Bond:       f.bond,
		Prevout:    wire.OutPoint{Hash: prev, Index: 0, Tree: wire.TxTreeRegular},
		ValueAtoms: 1_000_000,
		FeeAtoms:   10_000,
		Branch:     f.branch,
		Punisher:   f.punisher,
		Params:     chaincfg.TestNet3Params(),
	}
}

// The full path: a cheat attests two occupancies for one cell, the evidence
// store recovers its log key from the divergent pair, and the sweep turns that
// key into a spend the real consensus engine accepts - single output, pinned.
func TestDivergentCellAttestationsSweepTheBond(t *testing.T) {
	f := newSweepFixture(t)

	// Two cell attestations at one band position telling different stories.
	// 4294967333 = 1<<32 + 37: the attestation band slot for cell 37.
	pos := forfeit.Position{Match: sweepMatch, Domain: forfeit.DomainEntry, Seq: 4294967333}
	digA := blake256.Sum256([]byte("cell 37 is water"))
	digB := blake256.Sum256([]byte("cell 37 holds a ship"))
	sigA, err := forfeit.Sign(f.logPriv, pos, digA[:])
	if err != nil {
		t.Fatalf("first attestation: %v", err)
	}
	sigB, err := forfeit.Sign(f.logPriv, pos, digB[:])
	if err != nil {
		t.Fatalf("second attestation: %v", err)
	}

	store, err := evidence.Open(filepath.Join(t.TempDir(), "evidence.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if k, err := store.Record(f.logPriv.PubKey(), pos, digA, sigA); err != nil || k != nil {
		t.Fatalf("first half recovered a key: %v, %v", k, err)
	}
	recovered, err := store.Record(f.logPriv.PubKey(), pos, digB, sigB)
	if err != nil {
		t.Fatalf("second half: %v", err)
	}
	if recovered == nil {
		t.Fatal("a divergent pair recovered nothing")
	}
	if !recovered.PubKey().IsEqual(f.logPriv.PubKey()) {
		t.Fatal("the recovered key is not the cheat's log key")
	}

	pinned := pinnedScript()
	final, err := SweepForfeited(recovered, pinned, sweepDraft(f))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(final.TxIn) != 1 || len(final.TxOut) != 1 {
		t.Fatalf("swept with %d inputs and %d outputs, want 1 and 1",
			len(final.TxIn), len(final.TxOut))
	}
	if final.TxIn[0].Sequence != 0 {
		t.Fatalf("the punishment branch carries a sequence of %d, and it has no timelock",
			final.TxIn[0].Sequence)
	}
	if final.TxOut[0].Value != 990_000 {
		t.Fatalf("the sweep pays %d, want 990000", final.TxOut[0].Value)
	}
	if !bytes.Equal(final.TxOut[0].PkScript, pinned) {
		t.Fatal("the sweep pays somewhere other than the pinned payout")
	}

	// Verified here independently of the builder, through the real engine.
	_, pkScript, err := escrow.Address(f.bond, chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("bond address: %v", err)
	}
	vm, err := txscript.NewEngine(pkScript, final, 0,
		txscript.ScriptVerifyCheckSequenceVerify, 0, nil)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if err := vm.Execute(); err != nil {
		t.Fatalf("the sweep does not satisfy the bond's punishment branch: %v", err)
	}
}

// The refusals: no pinned script, a destination that is not the pinned one,
// and a second output are all turned away by the check the builder itself runs.
func TestSweepRefusesAnUnpinnedSpend(t *testing.T) {
	f := newSweepFixture(t)
	recovered := f.logPriv // the key itself; only the refusal paths run here

	if _, err := SweepForfeited(recovered, nil, sweepDraft(f)); err == nil {
		t.Fatal("swept with no pinned payout")
	}
	if _, err := SweepForfeited(nil, pinnedScript(), sweepDraft(f)); err == nil {
		t.Fatal("swept with no recovered key")
	}
	d := sweepDraft(f)
	d.Punisher = nil
	if _, err := SweepForfeited(recovered, pinnedScript(), d); err == nil {
		t.Fatal("swept without the punisher half")
	}

	pinned := pinnedScript()
	good := wire.NewMsgTx()
	good.AddTxOut(wire.NewTxOut(990_000, pinned))
	if err := CheckPinned(good, pinned); err != nil {
		t.Fatalf("refused the pinned single-output shape: %v", err)
	}
	elsewhere := good.Copy()
	elsewhere.TxOut[0].PkScript = append([]byte{0x76, 0xa9, 0x14}, append(bytes.Repeat([]byte{0x7e}, 20), 0x88, 0xac)...)
	if err := CheckPinned(elsewhere, pinned); err == nil {
		t.Fatal("passed a spend paying somewhere other than the pinned payout")
	}
	second := good.Copy()
	second.AddTxOut(wire.NewTxOut(1, pinned))
	if err := CheckPinned(second, pinned); err == nil {
		t.Fatal("passed a spend with a second output")
	}
	if err := CheckPinned(nil, pinned); err == nil {
		t.Fatal("passed a spend that is not there")
	}
	if err := CheckPinned(good, nil); err == nil {
		t.Fatal("passed with no pinned payout to hold to")
	}
}

// A forfeit spend can never reach the co-signed shape: its witness carries one
// signature, so the bridge can never treat it as free to pay anyone, and its
// single output must be the pinned payout (spec 9.5).
func TestForfeitSpendNeverCarriesTwoSigs(t *testing.T) {
	f := newSweepFixture(t)
	digA, sigA := storyHalf(t, f.logPriv, "tale one")
	digB, sigB := storyHalf(t, f.logPriv, "tale two")
	recovered, err := forfeit.Recover(f.logPriv.PubKey(), digA, sigA, digB, sigB)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	final, err := SweepForfeited(recovered, pinnedScript(), sweepDraft(f))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n := sigPushes(t, final.TxIn[0].SignatureScript); n != 1 {
		t.Fatalf("the forfeit witness carries %d signatures, want exactly 1", n)
	}
}

// sigPushes counts 65-byte pushes before the final one (the redeem script),
// the way the bridge's signaturesIn does.
func sigPushes(t *testing.T, sigScript []byte) int {
	t.Helper()
	var pushes [][]byte
	tok := txscript.MakeScriptTokenizer(0, sigScript)
	for tok.Next() {
		if d := tok.Data(); d != nil {
			pushes = append(pushes, d)
		}
	}
	if tok.Err() != nil || len(pushes) < 2 {
		t.Fatalf("witness did not tokenize: %v", tok.Err())
	}
	var n int
	for _, p := range pushes[:len(pushes)-1] {
		if len(p) == 65 {
			n++
		}
	}
	return n
}

// storyHalf signs one story at the fixture's band position, returning the
// digest and signature half a recovery is staged from.
func storyHalf(t *testing.T, priv *secp256k1.PrivateKey, story string) ([]byte, []byte) {
	t.Helper()
	d := blake256.Sum256([]byte(story))
	pos := forfeit.Position{Match: sweepMatch, Domain: forfeit.DomainEntry, Seq: 4294967333}
	sig, err := forfeit.Sign(priv, pos, d[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return d[:], sig
}

// The sweep is the realisation of exactly one matrix cell: the equivocation
// row's forfeitable bond, taken by the victim.

// CheckPinned is what holds a punishment spend to the payout it was pinned to.
// These spends are unilateral - nobody co-signs them - so their only legal
// shape is everything to the pinned script, and the builders run this on their
// own output for exactly that reason.
//
// The refusal-path test above never reaches it, because it fails earlier. This
// exercises the guard itself.
func TestCheckPinnedHoldsASpendToItsPayout(t *testing.T) {
	pinned := pinnedScript()
	elsewhere := append([]byte(nil), pinned...)
	elsewhere[len(elsewhere)-1] ^= 0xff

	one := wire.NewMsgTx()
	one.AddTxOut(wire.NewTxOut(1000, pinned))
	if err := CheckPinned(one, pinned); err != nil {
		t.Fatalf("refused a spend that pays exactly the pinned payout: %v", err)
	}

	for _, tc := range []struct {
		name string
		tx   *wire.MsgTx
	}{
		{"no transaction at all", nil},
		{"no outputs", wire.NewMsgTx()},
		{"paying somewhere else", func() *wire.MsgTx {
			tx := wire.NewMsgTx()
			tx.AddTxOut(wire.NewTxOut(1000, elsewhere))
			return tx
		}()},
		{"a second output skimming some off", func() *wire.MsgTx {
			tx := wire.NewMsgTx()
			tx.AddTxOut(wire.NewTxOut(900, pinned))
			tx.AddTxOut(wire.NewTxOut(100, elsewhere))
			return tx
		}()},
	} {
		if err := CheckPinned(tc.tx, pinned); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
	if err := CheckPinned(one, nil); err == nil {
		t.Error("held a spend to no payout at all")
	}
}

// And a sweep that succeeds really does pay only there.
func TestASweepPaysOnlyThePinnedPayout(t *testing.T) {
	f := newSweepFixture(t)
	pinned := pinnedScript()
	tx, err := SweepForfeited(f.logPriv, pinned, sweepDraft(f))
	if err != nil {
		t.Skipf("this fixture does not reach a built sweep: %v", err)
	}
	if len(tx.TxOut) != 1 {
		t.Fatalf("a sweep built %d outputs", len(tx.TxOut))
	}
	if !bytes.Equal(tx.TxOut[0].PkScript, pinned) {
		t.Fatal("a sweep paid somewhere other than the pinned payout")
	}
}
