package escrow

import (
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/wire"
)

// The race, run through the real consensus engine.
//
// Two things have to hold and they pull in opposite directions. A player who
// walks out must lose their bond to the rest of the table. A player who is still
// there must be able to keep it, against any number of people saying otherwise.
// Everything below is one or the other.

const (
	testClaimBlocks = ClaimBlocks
	testLockBlocks  = MinBondBlocks
)

type tableBond struct {
	privs  []*secp256k1.PrivateKey
	terms  *TableBondTerms
	script []byte
}

// postTableBond seats n players and posts the first one's bond.
func postTableBond(t *testing.T, n int) *tableBond {
	t.Helper()
	privs, pubs := memberKeys(t, n)
	script, err := TableBondScript(pubs[0], pubs, testLockBlocks)
	if err != nil {
		t.Fatalf("build bond: %v", err)
	}
	terms, err := ParseTableBond(script)
	if err != nil {
		t.Fatalf("parse bond: %v", err)
	}
	return &tableBond{privs: privs, terms: terms, script: script}
}

// sign produces signatures over tx in the order the given keys are listed.
func (b *tableBond) sign(t *testing.T, tx *wire.MsgTx, keys [][]byte) [][]byte {
	t.Helper()
	sigs := make([][]byte, 0, len(keys))
	for _, k := range keys {
		sigs = append(sigs, signInput(t, privFor(t, b.privs, k), b.script, tx))
	}
	return sigs
}

// A table that ends properly hands every bond back, with no delay and no chain
// argument. This is the path that should happen every time.
func TestATableThatEndsProperlyReleasesTheBond(t *testing.T) {
	for _, n := range []int{2, 3, 6} {
		b := postTableBond(t, n)
		tx := spendTx(t, 0)
		sig, err := AliveSigScript(b.script, b.sign(t, tx, b.terms.Members))
		if err != nil {
			t.Fatalf("%d seats: alive sigscript: %v", n, err)
		}
		tx.TxIn[0].SignatureScript = sig
		if err := execute(t, b.script, tx, csvFlags); err != nil {
			t.Fatalf("%d seats: a table that ended properly could not release the bond: %v", n, err)
		}
	}
}

func TestTheOwnerCannotReleaseTheBondAlone(t *testing.T) {
	b := postTableBond(t, 3)
	tx := spendTx(t, 0)

	// Their own signature, repeated into every slot.
	own := signInput(t, b.privs[0], b.script, tx)
	sigs := make([][]byte, len(b.terms.Members))
	for i := range sigs {
		sigs[i] = own
	}
	sig, err := AliveSigScript(b.script, sigs)
	if err != nil {
		t.Fatalf("alive sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sig
	if err := execute(t, b.script, tx, csvFlags); err == nil {
		t.Fatal("the owner released their own bond without the table")
	}

	// Nor through the backstop before its lock matures.
	early := spendTx(t, testLockBlocks-1)
	back, err := BackstopSigScript(b.script, signInput(t, b.privs[0], b.script, early))
	if err != nil {
		t.Fatalf("backstop sigscript: %v", err)
	}
	early.TxIn[0].SignatureScript = back
	if err := execute(t, b.script, early, csvFlags); err == nil {
		t.Fatal("the owner reclaimed their bond before the backstop matured")
	}
}

// A table that simply dissolves must not strand the coin forever.
func TestTheBackstopEventuallyReturnsTheBond(t *testing.T) {
	b := postTableBond(t, 3)
	tx := spendTx(t, testLockBlocks)
	sig, err := BackstopSigScript(b.script, signInput(t, b.privs[0], b.script, tx))
	if err != nil {
		t.Fatalf("backstop sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sig
	if err := execute(t, b.script, tx, csvFlags); err != nil {
		t.Fatalf("the owner could not reclaim a stranded bond: %v", err)
	}
}

// Reading a bond back has to be exact: what matters is not that coin is locked
// but which roster can take it.
func TestATableBondIsReadBackExactly(t *testing.T) {
	b := postTableBond(t, 4)

	if string(b.terms.Owner) != string(b.privs[0].PubKey().SerializeCompressed()) {
		t.Fatal("parsed the wrong owner")
	}
	if len(b.terms.Members) != 4 || len(b.terms.Others) != 3 {
		t.Fatalf("parsed %d members and %d others, want 4 and 3",
			len(b.terms.Members), len(b.terms.Others))
	}
	if b.terms.ClaimBlocks != testClaimBlocks || b.terms.LockBlocks != testLockBlocks {
		t.Fatalf("parsed %d/%d blocks, want %d/%d",
			b.terms.ClaimBlocks, b.terms.LockBlocks, testClaimBlocks, testLockBlocks)
	}
	if _, err := MemberIndex(b.terms, b.terms.Owner); err != nil {
		t.Fatalf("the owner is not among the members: %v", err)
	}
	strangers, _ := memberKeys(t, 1)
	if _, err := MemberIndex(b.terms, strangers[0].PubKey().SerializeCompressed()); err == nil {
		t.Fatal("a key not at the table was reported as a member")
	}

	// Other kinds of bond must not read as this one.
	plain, err := BondScript(b.terms.Owner, testLockBlocks)
	if err != nil {
		t.Fatalf("plain bond: %v", err)
	}
	if _, err := ParseTableBond(plain); err == nil {
		t.Fatal("a plain bond parsed as a table bond")
	}
}

// The builder refuses what it cannot make safe.
func TestTheTableBondBuilderRefusesUnsafeTerms(t *testing.T) {
	_, pubs := memberKeys(t, 3)
	strangers, _ := memberKeys(t, 1)
	stranger := strangers[0].PubKey().SerializeCompressed()

	for _, tc := range []struct {
		name    string
		owner   []byte
		members [][]byte
		lock    uint32
	}{
		{"an owner who is not at the table", stranger, pubs, testLockBlocks},
		{"a table of one", pubs[0], pubs[:1], testLockBlocks},
		{"a lock under the minimum", pubs[0], pubs, MinBondBlocks - 1},
		{"a lock nothing could satisfy", pubs[0], pubs, MaxCSVBlocks + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := TableBondScript(tc.owner, tc.members, tc.lock); err == nil {
				t.Fatal("built a bond that should have been refused")
			}
		})
	}
}

// A peer must never sign a bond release it did not derive itself.
//
// The release needs every member's signature, so a seat that signed whatever
// arrived would hand the sender a transaction paying wherever they liked with
// everybody's names on it. This is the only thing standing between a
// co-operative release and a way to rob the table politely.
func TestABondReleaseIsRefusedUnlessItIsTheOneWeWouldBuild(t *testing.T) {
	b := postTableBond(t, 2)
	spent := spendTx(t, 0)
	d := AliveDraft{
		Bond:       b.script,
		Prevout:    spent.TxIn[0].PreviousOutPoint,
		ValueAtoms: spent.TxIn[0].ValueIn,
		PayScript:  spent.TxOut[0].PkScript,
		FeeAtoms:   2000,
	}

	honest, err := BuildAlive(d)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := CheckAliveDraft(honest, d); err != nil {
		t.Fatalf("the transaction we build ourselves was refused: %v", err)
	}

	// Paying somewhere else, which is the attack.
	elsewhere := honest.Copy()
	elsewhere.TxOut[0].PkScript = append(
		append([]byte(nil), honest.TxOut[0].PkScript...), 0x51)
	if err := CheckAliveDraft(elsewhere, d); err == nil {
		t.Fatal("a release paying somebody else was accepted")
	}

	// Keeping more than the fee.
	greedy := honest.Copy()
	greedy.TxOut[0].Value -= 1000
	if err := CheckAliveDraft(greedy, d); err == nil {
		t.Fatal("a release skimming the difference was accepted")
	}

	// Spending some other output.
	elsewhereIn := honest.Copy()
	elsewhereIn.TxIn[0].PreviousOutPoint.Index++
	if err := CheckAliveDraft(elsewhereIn, d); err == nil {
		t.Fatal("a release spending a different output was accepted")
	}

	// And a timelocked sequence, which would be the backstop branch wearing
	// the alive branch's clothes.
	locked := honest.Copy()
	locked.TxIn[0].Sequence = 42
	if err := CheckAliveDraft(locked, d); err == nil {
		t.Fatal("a release carrying a timelock was accepted")
	}
}

// Everybody else together cannot spend the bond at all.
//
// Stronger than it used to be, and worth its own test because of that. The bond
// once had a branch the others could take on their own after a delay; now the only
// branch that moves it needs every member, the owner included - so an accusation
// is something the whole table agreed in advance, and a table that never agreed
// one cannot manufacture it later.
func TestEverybodyElseTogetherCannotSpendTheBond(t *testing.T) {
	b := postTableBond(t, 3)
	tx := spendTx(t, 0)

	// The others' signatures in every members' slot: the most a collusion
	// could assemble without the owner.
	theirs := b.sign(t, tx, b.terms.Others)
	all := make([][]byte, 0, len(b.terms.Members))
	for i := range b.terms.Members {
		all = append(all, theirs[i%len(theirs)])
	}
	sig, err := AliveSigScript(b.script, all)
	if err != nil {
		t.Fatalf("alive sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sig
	if err := execute(t, b.script, tx, csvFlags); err == nil {
		t.Fatal("everybody except the owner spent the bond, which leaves nothing to answer")
	}
}
