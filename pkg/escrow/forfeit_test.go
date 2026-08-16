package escrow

import (
	"bytes"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
)

// These drive the real consensus engine, because a script that looks right is
// worth nothing - the only opinion that counts is the one that will be applied
// when the transaction is broadcast.
//
// The bond has to hold in both directions and the two are equally important. A
// punishment branch that an honest owner's opponent can take at will is worse
// than no bond; a branch a cheat can dodge is decoration.

const testBondLock = MinBondBlocks

// twoPartyKey builds a key with a known secret that neither of two named
// parties can produce alone, which is all a script test needs. It is
// deliberately not the rule a real branch key is built with - see
// pkg/forfeit.ForfeitKey - so that nothing in this file can quietly become a
// second copy of that rule and drift from it while both keep passing. The one
// test that does tie the two together is below.
func twoPartyKey(t *testing.T, a, b *secp256k1.PrivateKey) (*secp256k1.PrivateKey, []byte) {
	t.Helper()
	two := new(secp256k1.ModNScalar).SetInt(2)
	d := new(secp256k1.ModNScalar).Mul2(two, &b.Key).Add(&a.Key)
	if d.IsZero() {
		t.Fatal("keys weighted to zero")
	}
	priv := secp256k1.NewPrivateKey(d)
	return priv, priv.PubKey().SerializeCompressed()
}

// bondSetup is a bond posted by an owner, forfeitable to n opponents.
type bondSetup struct {
	owner    *secp256k1.PrivateKey
	ownerPub []byte
	log      *secp256k1.PrivateKey   // the key that leaks if the owner equivocates
	punish   []*secp256k1.PrivateKey // one per opponent
	spend    []*secp256k1.PrivateKey // what each branch needs, via twoPartyKey
	forfeit  [][]byte
	script   []byte
}

func postBond(t *testing.T, n int) *bondSetup {
	t.Helper()
	privs, pubs := memberKeys(t, 2+n)
	s := &bondSetup{owner: privs[0], ownerPub: pubs[0], log: privs[1]}
	for i := range n {
		p := privs[2+i]
		spend, pub := twoPartyKey(t, s.log, p)
		s.punish = append(s.punish, p)
		s.spend = append(s.spend, spend)
		s.forfeit = append(s.forfeit, pub)
	}
	script, err := ForfeitableBondScript(s.ownerPub, s.forfeit, testBondLock)
	if err != nil {
		t.Fatalf("build bond: %v", err)
	}
	s.script = script
	return s
}

// The owner gets their bond back after the lock, having done nothing wrong.
func TestAnHonestOwnerReclaimsTheBond(t *testing.T) {
	for _, n := range []int{1, 2, 5} {
		s := postBond(t, n)
		tx := spendTx(t, testBondLock)
		sig, err := ForfeitableRefundSigScript(s.script, signInput(t, s.owner, s.script, tx))
		if err != nil {
			t.Fatalf("%d opponents: refund sigscript: %v", n, err)
		}
		tx.TxIn[0].SignatureScript = sig
		if err := execute(t, s.script, tx, csvFlags); err != nil {
			t.Fatalf("%d opponents: an honest owner could not reclaim their bond: %v", n, err)
		}
	}
}

// And not before the lock matures.
func TestTheBondCannotBeReclaimedEarly(t *testing.T) {
	s := postBond(t, 2)
	tx := spendTx(t, testBondLock-1)
	sig, err := ForfeitableRefundSigScript(s.script, signInput(t, s.owner, s.script, tx))
	if err != nil {
		t.Fatalf("refund sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sig
	if err := execute(t, s.script, tx, csvFlags); err == nil {
		t.Fatal("the bond was reclaimed a block before its lock matured")
	}
}

// The punishment: once the owner's log key is public, the opponent it was
// combined with takes the bond, with no timelock to wait for.
func TestAWrongedOpponentTakesTheBondImmediately(t *testing.T) {
	for _, n := range []int{1, 2, 5} {
		s := postBond(t, n)
		for i := range n {
			tx := spendTx(t, 0)
			sig, err := ForfeitSigScript(s.script, signInput(t, s.spend[i], s.script, tx), i)
			if err != nil {
				t.Fatalf("%d opponents, branch %d: sigscript: %v", n, i, err)
			}
			tx.TxIn[0].SignatureScript = sig
			if err := execute(t, s.script, tx, csvFlags); err != nil {
				t.Fatalf("%d opponents: branch %d could not take the bond: %v", n, i, err)
			}
		}
	}
}

// The direction that protects the honest player: an opponent who was never
// wronged holds only their own half and can do nothing with it.
func TestAnOpponentCannotTakeTheBondWithoutTheLeakedKey(t *testing.T) {
	s := postBond(t, 3)

	for i, punisher := range s.punish {
		tx := spendTx(t, 0)
		sig, err := ForfeitSigScript(s.script, signInput(t, punisher, s.script, tx), i)
		if err != nil {
			t.Fatalf("sigscript: %v", err)
		}
		tx.TxIn[0].SignatureScript = sig
		if err := execute(t, s.script, tx, csvFlags); err == nil {
			t.Fatalf("opponent %d took the bond with only their own key", i)
		}
	}
}

// And the direction that protects the punishment: the owner knows the log key,
// but that alone does not open any punishment branch, so they cannot dodge the
// timelock by taking their own bond back early.
func TestTheOwnerCannotUseAPunishmentBranchToDodgeTheLock(t *testing.T) {
	s := postBond(t, 3)

	for _, k := range []*secp256k1.PrivateKey{s.owner, s.log} {
		for i := range s.forfeit {
			tx := spendTx(t, 0)
			sig, err := ForfeitSigScript(s.script, signInput(t, k, s.script, tx), i)
			if err != nil {
				t.Fatalf("sigscript: %v", err)
			}
			tx.TxIn[0].SignatureScript = sig
			if err := execute(t, s.script, tx, csvFlags); err == nil {
				t.Fatalf("the owner reclaimed their bond early through punishment branch %d", i)
			}
		}
	}
}

// One opponent's leaked key must not open another opponent's branch. Each
// punishment is directed at exactly one player.
func TestOnePunishmentBranchDoesNotOpenAnother(t *testing.T) {
	s := postBond(t, 3)

	for i := range s.spend {
		for j := range s.forfeit {
			if i == j {
				continue
			}
			tx := spendTx(t, 0)
			sig, err := ForfeitSigScript(s.script, signInput(t, s.spend[i], s.script, tx), j)
			if err != nil {
				t.Fatalf("sigscript: %v", err)
			}
			tx.TxIn[0].SignatureScript = sig
			if err := execute(t, s.script, tx, csvFlags); err == nil {
				t.Fatalf("branch %d's key opened branch %d", i, j)
			}
		}
	}
}

// A bystander who watched the same equivocation holds the leaked key and can
// still do nothing: every branch needs a punishment key they do not have.
func TestABystanderCannotTakeAForfeitedBond(t *testing.T) {
	s := postBond(t, 2)
	outsiders, _ := memberKeys(t, 1)
	theirs, _ := twoPartyKey(t, s.log, outsiders[0])

	for i := range s.forfeit {
		tx := spendTx(t, 0)
		sig, err := ForfeitSigScript(s.script, signInput(t, theirs, s.script, tx), i)
		if err != nil {
			t.Fatalf("sigscript: %v", err)
		}
		tx.TxIn[0].SignatureScript = sig
		if err := execute(t, s.script, tx, csvFlags); err == nil {
			t.Fatalf("a bystander took the bond through branch %d", i)
		}
	}
}

// The one test that ties the two packages together. Everything else in this
// file is about the script and holds for any key nobody can sign for alone;
// this one is about the key the script is really built from.
func TestARealForfeitKeySpendsAPunishmentBranch(t *testing.T) {
	privs, pubs := memberKeys(t, 3)
	ownerPub := pubs[0]
	logPriv, punisher := privs[1], privs[2]

	br := forfeit.Branch{
		Match: "9bbccbcc99e2421852775868835efd6926eab532fb3286f1051f79f7572bb9b9",
		Seat:  pubs[2],
	}
	fPub, err := forfeit.ForfeitKey(br, logPriv.PubKey(), punisher.PubKey())
	if err != nil {
		t.Fatalf("build the branch key: %v", err)
	}
	fKey := fPub.SerializeCompressed()

	// The two rules must stay apart, pinned one against the other. If the real
	// key ever matched this file's test-only rule, or collapsed back to the
	// plain sum, every test here would move in step with a regression there
	// and neither side would notice.
	if _, testRule := twoPartyKey(t, logPriv, punisher); bytes.Equal(fKey, testRule) {
		t.Fatal("the real branch key matches this file's test-only rule")
	}
	plainSum := new(secp256k1.ModNScalar).Set(&logPriv.Key).Add(&punisher.Key)
	if bytes.Equal(fKey, secp256k1.NewPrivateKey(plainSum).PubKey().SerializeCompressed()) {
		t.Fatal("the real branch key is the plain sum of its halves, which a chosen punishment key cancels")
	}

	script, err := ForfeitableBondScript(ownerPub, [][]byte{fKey}, testBondLock)
	if err != nil {
		t.Fatalf("build bond: %v", err)
	}
	terms, err := ParseForfeitableBond(script)
	if err != nil {
		t.Fatalf("parse bond: %v", err)
	}
	index, err := ForfeitIndex(terms, fKey)
	if err != nil {
		t.Fatalf("the bond has no branch for the key it was built with: %v", err)
	}

	spend, err := forfeit.ForfeitPrivKey(br, logPriv, punisher)
	if err != nil {
		t.Fatalf("derive the spending key: %v", err)
	}
	tx := spendTx(t, 0)
	sig, err := ForfeitSigScript(script, signInput(t, spend, script, tx), index)
	if err != nil {
		t.Fatalf("sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sig
	if err := execute(t, script, tx, csvFlags); err != nil {
		t.Fatalf("a real forfeit key could not take the bond it was built into: %v", err)
	}

	// A branch rebuilt wrongly at recovery time derives a key that is in no
	// script, and it fails where the docs promise: found by nothing, signing
	// for nothing.
	wrong := forfeit.Branch{
		Match: "3f0c7a1e55d9b84406e2c1fd7ab399215c6e80d4488f13ba0dd5e97c22461af8",
		Seat:  pubs[2],
	}
	wrongSpend, err := forfeit.ForfeitPrivKey(wrong, logPriv, punisher)
	if err != nil {
		t.Fatalf("derive from the wrong branch: %v", err)
	}
	if _, err := ForfeitIndex(terms, wrongSpend.PubKey().SerializeCompressed()); err == nil {
		t.Fatal("a key derived from the wrong branch was found in the bond")
	}
	tx = spendTx(t, 0)
	sig, err = ForfeitSigScript(script, signInput(t, wrongSpend, script, tx), index)
	if err != nil {
		t.Fatalf("sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sig
	if err := execute(t, script, tx, csvFlags); err == nil {
		t.Fatal("a key derived from the wrong branch took the bond")
	}
}

// Reading a bond back has to be exact, because the thing a peer must check is
// not that coin is locked but whose key can take it.
func TestAForfeitableBondIsReadBackExactly(t *testing.T) {
	s := postBond(t, 3)

	terms, err := ParseForfeitableBond(s.script)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if string(terms.Owner) != string(s.ownerPub) {
		t.Fatal("parsed the wrong owner")
	}
	if terms.LockBlocks != testBondLock {
		t.Fatalf("parsed a lock of %d, want %d", terms.LockBlocks, testBondLock)
	}
	if len(terms.Forfeit) != len(s.forfeit) {
		t.Fatalf("parsed %d punishment branches, want %d", len(terms.Forfeit), len(s.forfeit))
	}
	for i := range s.forfeit {
		if string(terms.Forfeit[i]) != string(s.forfeit[i]) {
			t.Fatalf("punishment branch %d parsed to the wrong key", i)
		}
		if got, err := ForfeitIndex(terms, s.forfeit[i]); err != nil || got != i {
			t.Fatalf("branch %d was found at %d (%v)", i, got, err)
		}
	}

	// The finding that must stop a game: a bond with no branch for me.
	strangers, _ := memberKeys(t, 1)
	if _, err := ForfeitIndex(terms, strangers[0].PubKey().SerializeCompressed()); err == nil {
		t.Fatal("a bond with no branch for a key reported one anyway")
	}

	// A plain bond is not a forfeitable one, and must not read as one.
	plain, err := BondScript(s.ownerPub, testBondLock)
	if err != nil {
		t.Fatalf("plain bond: %v", err)
	}
	if _, err := ParseForfeitableBond(plain); err == nil {
		t.Fatal("a plain bond parsed as a forfeitable one")
	}
}

// A script that merely contains the right pushes must not pass as a bond. This
// is what rebuilding-and-comparing buys over reading opcode by opcode.
func TestAScriptThatOnlyLooksLikeABondIsRejected(t *testing.T) {
	s := postBond(t, 1)

	// The same terms, plus a quiet extra way to the coin.
	sneaky, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_IF).
		AddData(s.forfeit[0]).
		AddInt64(sigType).
		AddOp(txscript.OP_CHECKSIGALTVERIFY).
		AddOp(txscript.OP_ELSE).
		AddInt64(int64(testBondLock)).
		AddOp(txscript.OP_CHECKSEQUENCEVERIFY).
		AddOp(txscript.OP_DROP).
		AddData(s.ownerPub).
		AddInt64(sigType).
		AddOp(txscript.OP_CHECKSIGALTVERIFY).
		AddOp(txscript.OP_ENDIF).
		AddOp(txscript.OP_TRUE).
		AddOp(txscript.OP_TRUE).
		Script()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := ParseForfeitableBond(sneaky); err == nil {
		t.Fatal("a script with an extra opcode parsed as a bond on these terms")
	}
}

// The builder has to refuse what it cannot make safe.
func TestTheBuilderRefusesUnsafeBonds(t *testing.T) {
	privs, pubs := memberKeys(t, 3)
	owner, a, b := pubs[0], pubs[1], pubs[2]
	_ = privs

	for _, tc := range []struct {
		name    string
		owner   []byte
		forfeit [][]byte
		lock    uint32
	}{
		{"nobody to forfeit to", owner, nil, testBondLock},
		{"the owner's own key as a punishment branch", owner, [][]byte{owner}, testBondLock},
		{"the same opponent twice", owner, [][]byte{a, a}, testBondLock},
		{"a lock under the minimum", owner, [][]byte{a}, MinBondBlocks - 1},
		{"a lock nothing could satisfy", owner, [][]byte{a}, MaxCSVBlocks + 1},
		{"too many opponents", owner, make([][]byte, MaxForfeitKeys+1), testBondLock},
		{"a malformed opponent key", owner, [][]byte{a, {0x02, 0x03}}, testBondLock},
		{"a malformed owner key", []byte{0x02}, [][]byte{a, b}, testBondLock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ForfeitableBondScript(tc.owner, tc.forfeit, tc.lock); err == nil {
				t.Fatal("built a bond that should have been refused")
			}
		})
	}

	// The largest lock that can actually be spent must stay allowed.
	if _, err := ForfeitableBondScript(owner, [][]byte{a}, MaxCSVBlocks); err != nil {
		t.Fatalf("refused the largest lock a bond can carry: %v", err)
	}
	// And a full table's worth of branches must fit in the push limit.
	full := make([][]byte, 0, MaxForfeitKeys)
	for range MaxForfeitKeys {
		k, _ := memberKeys(t, 1)
		full = append(full, k[0].PubKey().SerializeCompressed())
	}
	script, err := ForfeitableBondScript(owner, full, testBondLock)
	if err != nil {
		t.Fatalf("refused a bond with the maximum opponents: %v", err)
	}
	t.Logf("a bond forfeitable to %d opponents is %d bytes", MaxForfeitKeys, len(script))
}
