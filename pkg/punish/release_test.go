package punish

import (
	"bytes"
	"testing"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/wire"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
)

// ownerScript is a P2PKH-shaped stand-in for the bond owner's own wallet,
// distinct from the pinned payout by construction.
func ownerScript() []byte {
	return append([]byte{0x76, 0xa9, 0x14}, append(bytes.Repeat([]byte{0x2f}, 20), 0x88, 0xac)...)
}

func releaseFixture(f ladderFixture) Release {
	var prev chainhash.Hash
	copy(prev[:], bytes.Repeat([]byte{0xcc}, chainhash.HashSize))
	return Release{
		Bond:       f.bond,
		Prevout:    wire.OutPoint{Hash: prev, Index: 0, Tree: wire.TxTreeRegular},
		ValueAtoms: 1_000_000,
		OwnerPay:   ownerScript(),
		FeeAtoms:   10_000,
		Params:     chaincfg.TestNet3Params(),
	}
}

// A cooperating table: both seats sign, the release finishes through the real
// engine, waits on nothing, and pays the owner - not a pinned sweep.
func TestCleanReleaseComesHome(t *testing.T) {
	f := newLadderFixture(t)
	r := releaseFixture(f)

	tx, err := BuildRelease(r)
	if err != nil {
		t.Fatalf("build release: %v", err)
	}
	sigs := make([][]byte, 0, 2)
	for _, m := range f.members {
		sig, err := escrow.SignBondSpend(tx, f.bond, f.keyFor(t, m))
		if err != nil {
			t.Fatalf("co-sign: %v", err)
		}
		sigs = append(sigs, sig)
	}
	final, err := CoSignRelease(tx, f.bond, sigs, r.Params)
	if err != nil {
		t.Fatalf("finish release: %v", err)
	}
	if n := sigPushes(t, final.TxIn[0].SignatureScript); n != 2 {
		t.Fatalf("the release carries %d signatures, want the co-signed 2", n)
	}
	if final.TxIn[0].Sequence != 0 {
		t.Fatalf("the release waits on a sequence of %d, and its branch has none", final.TxIn[0].Sequence)
	}
	if len(final.TxOut) != 1 || final.TxOut[0].Value != 990_000 {
		t.Fatalf("the release pays %d, want the whole bond less the fee, 990000", final.TxOut[0].Value)
	}
	if !bytes.Equal(final.TxOut[0].PkScript, ownerScript()) {
		t.Fatal("the release pays somewhere other than the owner's own script")
	}
	if bytes.Equal(final.TxOut[0].PkScript, pinnedScript()) {
		t.Fatal("the release swept to the pinned payout; a clean end takes nothing")
	}
}

// A withheld co-sign: one signature cannot finish the alive branch, and the
// owner falls back to the backstop, alone, at the bond's own lock.
func TestWithheldCoSignFallsToTheBackstop(t *testing.T) {
	f := newLadderFixture(t)
	r := releaseFixture(f)

	tx, err := BuildRelease(r)
	if err != nil {
		t.Fatalf("build release: %v", err)
	}
	ownerSig, err := escrow.SignBondSpend(tx, f.bond, f.keyA)
	if err != nil {
		t.Fatalf("owner sign: %v", err)
	}
	if _, err := CoSignRelease(tx, f.bond, [][]byte{ownerSig}, r.Params); err == nil {
		t.Fatal("one signature finished the two-seat alive branch")
	}

	back, err := BackstopRelease(f.keyA, r)
	if err != nil {
		t.Fatalf("backstop: %v", err)
	}
	if back.TxIn[0].Sequence != 4032 {
		t.Fatalf("the backstop carries a sequence of %d, want the bond lock of 4032", back.TxIn[0].Sequence)
	}
	if n := sigPushes(t, back.TxIn[0].SignatureScript); n != 1 {
		t.Fatalf("the backstop carries %d signatures, want the owner's 1", n)
	}
	if len(back.TxOut) != 1 || back.TxOut[0].Value != 990_000 {
		t.Fatalf("the backstop pays %d, want the whole bond less the fee, 990000", back.TxOut[0].Value)
	}
	if !bytes.Equal(back.TxOut[0].PkScript, ownerScript()) {
		t.Fatal("the backstop pays somewhere other than the owner's own script")
	}

	// The other seat has no way out through somebody else's backstop.
	if _, err := BackstopRelease(f.keyB, r); err == nil {
		t.Fatal("the other seat left through the owner's backstop")
	}
}

// The release is the same two-seat machine as the ladder: a wider bond is
// refused before anything could be signed.
func TestReleaseRefusesAWiderTable(t *testing.T) {
	f := newLadderFixture(t)
	keyC := testKey(t, 0xdd)
	three := [][]byte{
		f.keyA.PubKey().SerializeCompressed(),
		f.keyB.PubKey().SerializeCompressed(),
		keyC.PubKey().SerializeCompressed(),
	}
	wideBond, err := escrow.TableBondScript(three[0], three, 4032)
	if err != nil {
		t.Fatalf("three-seat bond: %v", err)
	}
	r := releaseFixture(f)
	r.Bond = wideBond
	if _, err := BuildRelease(r); err == nil {
		t.Fatal("built a release for a three-seat table")
	}
	if _, err := BackstopRelease(f.keyA, r); err == nil {
		t.Fatal("built a backstop for a three-seat table")
	}
}

// The release realises exactly one matrix cell: the clean-win row's table
// bond, home cooperatively or through the backstop.
