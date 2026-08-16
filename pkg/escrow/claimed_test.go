package escrow

import (
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/wire"
)

// The window has to start when the accusation does, and these are the four
// statements that makes true. Each one runs the consensus engine, because nothing
// else is an authority on what a script does.

type claimedBond struct {
	privs  []*secp256k1.PrivateKey
	terms  *ClaimedBondTerms
	script []byte
}

func claimedInto(t *testing.T, n int) *claimedBond {
	t.Helper()
	privs, pubs := memberKeys(t, n)
	script, err := ClaimedBondScript(pubs[0], pubs, testClaimBlocks)
	if err != nil {
		t.Fatalf("build claimed bond: %v", err)
	}
	terms, err := ParseClaimedBond(script)
	if err != nil {
		t.Fatalf("parse claimed bond: %v", err)
	}
	return &claimedBond{privs: privs, terms: terms, script: script}
}

func (b *claimedBond) sign(t *testing.T, tx *wire.MsgTx, keys [][]byte) [][]byte {
	t.Helper()
	sigs := make([][]byte, 0, len(keys))
	for _, k := range keys {
		sigs = append(sigs, signInput(t, privFor(t, b.privs, k), b.script, tx))
	}
	return sigs
}

// The answer, and the whole point: one key, no delay, nobody asked.
func TestAnAccusedSeatAnswersWithItsOwnKeyAlone(t *testing.T) {
	for _, n := range []int{2, 3, 6} {
		b := claimedInto(t, n)
		// Sequence zero: no wait at all.
		tx := spendTx(t, 0)
		sig, err := AnswerSigScript(b.script, b.sign(t, tx, [][]byte{b.terms.Owner})[0])
		if err != nil {
			t.Fatalf("%d seats: answer sigscript: %v", n, err)
		}
		tx.TxIn[0].SignatureScript = sig
		if err := execute(t, b.script, tx, csvFlags); err != nil {
			t.Fatalf("%d seats: an accused seat could not answer with its own key: %v", n, err)
		}
	}
}

// And the accusers wait, which is what gives the answer its priority. Not
// speed - the other branch simply cannot be taken yet.
func TestTheAccusersCannotTakeAClaimedBondBeforeTheWindow(t *testing.T) {
	b := claimedInto(t, 3)
	tx := spendTx(t, uint32(testClaimBlocks)-1)
	sig, err := TakeSigScript(b.script, b.sign(t, tx, b.terms.Others))
	if err != nil {
		t.Fatalf("take sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sig
	if err := execute(t, b.script, tx, csvFlags); err == nil {
		t.Fatal("a claimed bond was taken a block before the window closed")
	}
}

// Once it has closed, a seat that never answered loses it.
func TestTheAccusersTakeAClaimedBondAfterTheWindow(t *testing.T) {
	for _, n := range []int{2, 3, 6} {
		b := claimedInto(t, n)
		tx := spendTx(t, uint32(testClaimBlocks))
		sig, err := TakeSigScript(b.script, b.sign(t, tx, b.terms.Others))
		if err != nil {
			t.Fatalf("%d seats: take sigscript: %v", n, err)
		}
		tx.TxIn[0].SignatureScript = sig
		if err := execute(t, b.script, tx, csvFlags); err != nil {
			t.Fatalf("%d seats: the table could not take an unanswered bond: %v", n, err)
		}
	}
}

// The owner cannot take the forfeiture branch, even after the window, and no
// proper subset of the others can take it either. Otherwise a seat could answer
// by taking its own bond out, or one accuser could act for the table.
func TestOnlyTheWholeTableTakesAnUnansweredBond(t *testing.T) {
	b := claimedInto(t, 3)
	tx := spendTx(t, uint32(testClaimBlocks))

	// The owner, in the others' slots.
	owner := b.sign(t, tx, [][]byte{b.terms.Owner})
	all := append([][]byte{}, owner...)
	for len(all) < len(b.terms.Others) {
		all = append(all, owner[0])
	}
	sig, err := TakeSigScript(b.script, all)
	if err != nil {
		t.Fatalf("take sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sig
	if err := execute(t, b.script, tx, csvFlags); err == nil {
		t.Fatal("the owner signed the forfeiture branch and the engine allowed it")
	}

	// One accuser's signature repeated, standing in for a subset acting alone.
	one := b.sign(t, tx, b.terms.Others[:1])
	subset := make([][]byte, 0, len(b.terms.Others))
	for range b.terms.Others {
		subset = append(subset, one[0])
	}
	sig, err = TakeSigScript(b.script, subset)
	if err != nil {
		t.Fatalf("take sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sig
	if err := execute(t, b.script, tx, csvFlags); err == nil {
		t.Fatal("one accuser took a bond the whole table has to agree on")
	}
}

// A script that merely looks like a claimed bond is refused, because the answer
// branch is the only thing standing between the accused and a forfeiture they
// cannot interrupt.
func TestALookalikeClaimedBondIsRefused(t *testing.T) {
	b := claimedInto(t, 3)

	// Somebody else's key in the answer branch: the coin is there, the shape
	// is right, and the owner could never answer.
	_, pubs := memberKeys(t, 3)
	wrong, err := ClaimedBondScript(pubs[0], pubs, testClaimBlocks)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := ParseClaimedBond(wrong); err != nil {
		t.Fatalf("a genuine claimed bond for other keys should still parse: %v", err)
	}
	if string(wrong) == string(b.script) {
		t.Fatal("two different tables produced one script")
	}

	// A trailing push is a second way to satisfy the script, and rebuilding is
	// what catches it.
	tampered := append(append([]byte(nil), b.script...), 0x51)
	if _, err := ParseClaimedBond(tampered); err == nil {
		t.Fatal("a claimed bond with something appended was read as genuine")
	}
}
