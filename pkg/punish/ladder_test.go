package punish

import (
	"bytes"
	"strings"
	"testing"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/wire"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
)

// The depth table the package doc publishes, pinned with literals: the gv1
// bond affords 49 rungs and the agreed cap of 8 binds instead; smaller bonds
// fall to the arithmetic; a bond of one minimum share affords nothing.
// The depth table, pinned at a fee of 10,000 atoms - dcrbattleships' gv1
// number, written as a literal so the table stays a fact about this code and
// not a restatement of whatever a game happens to choose.
func TestLadderDepthTable(t *testing.T) {
	const fee = 10_000
	for _, tc := range []struct {
		bond int64
		want int
	}{
		{1_000_000, 8},
		{180_000, 8},
		{160_000, 7},
		{60_000, 2},
		{40_000, 1},
		{20_000, 0},
		{0, 0},
	} {
		if got := LadderDepth(tc.bond, fee); got != tc.want {
			t.Fatalf("LadderDepth(%d, %d) = %d, want %d", tc.bond, fee, got, tc.want)
		}
	}
}

// A bigger fee affords fewer rungs, which is the whole reason the fee is a
// parameter: a game that raises it shortens its own ladder.
func TestABiggerFeeAffordsFewerRungs(t *testing.T) {
	cheap := LadderDepth(100_000, 10_000)
	dear := LadderDepth(100_000, 40_000)
	if dear >= cheap {
		t.Fatalf("at 40,000 the depth is %d and at 10,000 it is %d", dear, cheap)
	}
}

// ladderFixture is a two-seat table bond: seat A owns it, seat B is the
// wronged accuser.
type ladderFixture struct {
	bond    []byte
	claimed []byte
	keyA    *secp256k1.PrivateKey
	keyB    *secp256k1.PrivateKey
	members [][]byte // canonical order, from the bond itself
}

func newLadderFixture(t *testing.T) ladderFixture {
	t.Helper()
	keyA := testKey(t, 0x88)
	keyB := testKey(t, 0x99)
	pubA := keyA.PubKey().SerializeCompressed()
	pubB := keyB.PubKey().SerializeCompressed()

	bond, err := escrow.TableBondScript(pubA, [][]byte{pubA, pubB}, 4032)
	if err != nil {
		t.Fatalf("table bond: %v", err)
	}
	terms, err := escrow.ParseTableBond(bond)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	claimed, err := escrow.ClaimedBondScript(pubA, terms.Members, 3)
	if err != nil {
		t.Fatalf("claimed bond: %v", err)
	}
	return ladderFixture{bond: bond, claimed: claimed, keyA: keyA, keyB: keyB, members: terms.Members}
}

// keyFor returns the fixture key holding one canonical member slot.
func (f ladderFixture) keyFor(t *testing.T, member []byte) *secp256k1.PrivateKey {
	t.Helper()
	for _, k := range []*secp256k1.PrivateKey{f.keyA, f.keyB} {
		if bytes.Equal(k.PubKey().SerializeCompressed(), member) {
			return k
		}
	}
	t.Fatal("no fixture key for that member")
	return nil
}

func (f ladderFixture) accuseDraft() escrow.AccuseDraft {
	var prev chainhash.Hash
	copy(prev[:], bytes.Repeat([]byte{0xaa}, chainhash.HashSize))
	return escrow.AccuseDraft{
		Bond:       f.bond,
		Prevout:    wire.OutPoint{Hash: prev, Index: 0, Tree: wire.TxTreeRegular},
		ValueAtoms: 1_000_000,
		FeeAtoms:   10_000,
		Params:     chaincfg.TestNet3Params(),
	}
}

// The whole ladder at two seats: the pre-signed chain, the co-signed first
// rung, the owner's single-sig answer back into the bond, the next rung
// already pointing at that answer, and the take when nothing answers.
func TestLadderRunsAtTwoSeats(t *testing.T) {
	f := newLadderFixture(t)
	params := chaincfg.TestNet3Params()
	d := f.accuseDraft()

	chain, err := BuildLadder(d)
	if err != nil {
		t.Fatalf("build ladder: %v", err)
	}
	if len(chain) != 8 {
		t.Fatalf("the ladder has %d rungs, want 8", len(chain))
	}

	// Both seats co-sign the first rung, in canonical member order.
	sigs := make([][]byte, 0, 2)
	for _, m := range f.members {
		sig, err := escrow.SignBondSpend(chain[0], f.bond, f.keyFor(t, m))
		if err != nil {
			t.Fatalf("co-sign: %v", err)
		}
		sigs = append(sigs, sig)
	}
	accuse, err := CoSignAccuse(chain[0], f.bond, sigs, params)
	if err != nil {
		t.Fatalf("finish accuse: %v", err)
	}
	if n := sigPushes(t, accuse.TxIn[0].SignatureScript); n != 2 {
		t.Fatalf("the accuse leg carries %d signatures, want the co-signed 2", n)
	}
	_, claimedPk, err := escrow.Address(f.claimed, params)
	if err != nil {
		t.Fatalf("claimed address: %v", err)
	}
	if accuse.TxOut[0].Value != 990_000 || !bytes.Equal(accuse.TxOut[0].PkScript, claimedPk) {
		t.Fatalf("the accusation pays %d somewhere unexpected", accuse.TxOut[0].Value)
	}

	// The owner answers alone, straight back into its own bond.
	answer, err := AnswerClaim(f.keyA, escrow.AnswerDraft{
		Claimed:    f.claimed,
		Bond:       f.bond,
		Prevout:    wire.OutPoint{Hash: accuse.TxHash(), Index: 0, Tree: wire.TxTreeRegular},
		ValueAtoms: 990_000,
		FeeAtoms:   10_000,
		Params:     params,
	})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if n := sigPushes(t, answer.TxIn[0].SignatureScript); n != 1 {
		t.Fatalf("the answer carries %d signatures, want the owner's 1", n)
	}
	if answer.TxIn[0].Sequence != 0 {
		t.Fatalf("the answer waits on a sequence of %d, and its branch has none", answer.TxIn[0].Sequence)
	}
	_, bondPk, err := escrow.Address(f.bond, params)
	if err != nil {
		t.Fatalf("bond address: %v", err)
	}
	if answer.TxOut[0].Value != 980_000 || !bytes.Equal(answer.TxOut[0].PkScript, bondPk) {
		t.Fatalf("the answer pays %d somewhere other than back into the bond", answer.TxOut[0].Value)
	}
	// A stranger's key cannot answer: the branch is the owner's alone.
	if _, err := AnswerClaim(f.keyB, escrow.AnswerDraft{
		Claimed:    f.claimed,
		Bond:       f.bond,
		Prevout:    wire.OutPoint{Hash: accuse.TxHash(), Index: 0, Tree: wire.TxTreeRegular},
		ValueAtoms: 990_000,
		FeeAtoms:   10_000,
		Params:     params,
	}); err == nil {
		t.Fatal("the accuser answered the owner's claim")
	}

	// The second rung was pre-signed against exactly that answer.
	want := wire.OutPoint{Hash: answer.TxHash(), Index: 0, Tree: wire.TxTreeRegular}
	if chain[1].TxIn[0].PreviousOutPoint != want {
		t.Fatal("rung 1 does not spend the bond where answering rung 0 puts it")
	}

	// No answer: the wronged seat takes the whole bond to the pinned wallet.
	pinned := pinnedScript()
	take, err := TakeExpired(f.keyB, pinned, Take{
		Claimed:    f.claimed,
		Prevout:    wire.OutPoint{Hash: accuse.TxHash(), Index: 0, Tree: wire.TxTreeRegular},
		ValueAtoms: 990_000,
		FeeAtoms:   10_000,
		Params:     params,
	})
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if take.TxIn[0].Sequence != 3 {
		t.Fatalf("the take carries a sequence of %d, want ClaimBlocks = 3", take.TxIn[0].Sequence)
	}
	if len(take.TxOut) != 1 || take.TxOut[0].Value != 980_000 || !bytes.Equal(take.TxOut[0].PkScript, pinned) {
		t.Fatal("the take is not the whole bond, single-output, to the pinned payout")
	}
	if n := sigPushes(t, take.TxIn[0].SignatureScript); n != 1 {
		t.Fatalf("the take carries %d signatures, want the wronged seat's 1", n)
	}

	// The owner cannot take its own claimed bond, and nowhere else can be paid.
	if _, err := TakeExpired(f.keyA, pinned, Take{
		Claimed:    f.claimed,
		Prevout:    wire.OutPoint{Hash: accuse.TxHash(), Index: 0, Tree: wire.TxTreeRegular},
		ValueAtoms: 990_000,
		FeeAtoms:   10_000,
		Params:     params,
	}); err == nil {
		t.Fatal("the owner took its own claimed bond")
	}
	if _, err := TakeExpired(f.keyB, nil, Take{Claimed: f.claimed}); err == nil {
		t.Fatal("took with no pinned payout")
	}
}

// The ladder is a two-seat machine: more members and unaffordable bonds are
// refused before anything could be signed.
func TestLadderRefusesWhatItCannotRun(t *testing.T) {
	f := newLadderFixture(t)
	keyC := testKey(t, 0xbb)
	three := [][]byte{
		f.keyA.PubKey().SerializeCompressed(),
		f.keyB.PubKey().SerializeCompressed(),
		keyC.PubKey().SerializeCompressed(),
	}
	wideBond, err := escrow.TableBondScript(three[0], three, 4032)
	if err != nil {
		t.Fatalf("three-seat bond: %v", err)
	}

	d := f.accuseDraft()
	d.Bond = wideBond
	if _, err := BuildLadder(d); err == nil {
		t.Fatal("built a ladder for a three-seat table")
	}
	d = f.accuseDraft()
	d.ValueAtoms = 20_000
	if _, err := BuildLadder(d); err == nil {
		t.Fatal("built a ladder no rung of which could be collected on")
	}
	wideClaimed, err := escrow.ClaimedBondScript(three[0], three, 3)
	if err != nil {
		t.Fatalf("three-seat claimed bond: %v", err)
	}
	if _, err := TakeExpired(f.keyB, pinnedScript(), Take{
		Claimed:    wideClaimed,
		ValueAtoms: 990_000,
		FeeAtoms:   10_000,
		Params:     chaincfg.TestNet3Params(),
	}); err == nil {
		t.Fatal("took a claimed bond that pays more seats than the pilot has")
	}
}

// The take realises exactly one matrix cell: the chain-silence row's table
// bond, taken by the wronged seat.
// A fee of nothing builds a chain the network will not relay.
func TestLadderRefusesAFeeOfNothing(t *testing.T) {
	f := newLadderFixture(t)
	for _, fee := range []int64{0, -1} {
		d := f.accuseDraft()
		d.FeeAtoms = fee
		err := BuildLadderErr(d)
		if err == nil {
			t.Errorf("built a ladder at a fee of %d", fee)
			continue
		}
		// AffordableDepth also returns nothing for a non-positive fee, so
		// the depth check would refuse this too. The guard exists for the
		// sentence, which tells a caller what is actually wrong.
		if !strings.Contains(err.Error(), "needs a fee") {
			t.Errorf("fee %d refused without saying why: %v", fee, err)
		}
	}
}

// A fee too large for the bond affords no rung, and that is refused rather
// than silently building an empty ladder.
func TestLadderRefusesAFeeTheBondCannotAfford(t *testing.T) {
	f := newLadderFixture(t)
	d := f.accuseDraft()
	d.FeeAtoms = d.ValueAtoms
	if _, err := BuildLadder(d); err == nil {
		t.Fatal("built a ladder the bond cannot pay for")
	}
}

// BuildLadderErr is BuildLadder's error alone, for tests that only care why.
func BuildLadderErr(d escrow.AccuseDraft) error {
	_, err := BuildLadder(d)
	return err
}
