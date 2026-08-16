package escrow

import (
	"bytes"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/wire"
)

const testBondBlocks = MinBondBlocks

// bondSpend builds a spend of a bond at the given input sequence.
func bondSpend(t *testing.T, bond []byte, priv *secp256k1.PrivateKey, sequence uint32) *wire.MsgTx {
	t.Helper()
	tx := spendTx(t, sequence)
	sig := signInput(t, priv, bond, tx)
	sigScript, err := BondSigScript(bond, sig)
	if err != nil {
		t.Fatalf("bond sig script: %v", err)
	}
	tx.TxIn[0].SignatureScript = sigScript
	return tx
}

// A bond is the owner's coin, locked. It has to come back to them once the
// lock matures, or nobody would post one.
func TestBondSpendsAfterLock(t *testing.T) {
	privs, pubs := memberKeys(t, 1)
	bond, err := BondScript(pubs[0], testBondBlocks)
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}

	tx := bondSpend(t, bond, privs[0], testBondBlocks)
	if err := execute(t, bond, tx, csvFlags); err != nil {
		t.Fatalf("bond should spend once its lock has matured: %v", err)
	}
}

// The lock is the entire cost of a bond. If it could be reclaimed early it
// would deter nothing.
func TestBondDoesNotSpendBeforeLock(t *testing.T) {
	privs, pubs := memberKeys(t, 1)
	bond, err := BondScript(pubs[0], testBondBlocks)
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}

	for _, seq := range []uint32{0, 1, testBondBlocks - 1} {
		tx := bondSpend(t, bond, privs[0], seq)
		if err := execute(t, bond, tx, csvFlags); err == nil {
			t.Fatalf("bond spent at sequence %d, under its %d block lock", seq, testBondBlocks)
		}
	}
}

// Nobody but the owner can ever take a bond - that is what makes it safe to
// post before there is any roster to trust.
func TestBondCannotBeTakenByAnyoneElse(t *testing.T) {
	privs, pubs := memberKeys(t, 2)
	bond, err := BondScript(pubs[0], testBondBlocks)
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}

	stranger := privFor(t, privs, pubs[1])
	tx := bondSpend(t, bond, stranger, testBondBlocks)
	if err := execute(t, bond, tx, csvFlags); err == nil {
		t.Fatalf("a bond must not be spendable by anyone but its owner")
	}
}

func TestBondScriptRejectsShortLocks(t *testing.T) {
	_, pubs := memberKeys(t, 1)
	for _, blocks := range []uint32{0, 1, MinBondBlocks - 1} {
		if _, err := BondScript(pubs[0], blocks); err == nil {
			t.Fatalf("expected a %d block lock to be refused", blocks)
		}
	}
	if _, err := BondScript(pubs[0][:32], MinBondBlocks); err == nil {
		t.Fatalf("expected a malformed owner key to be refused")
	}
}

// A referee takes a player's word for which outpoint is their bond, so it has
// to be able to tell a real bond from a script that merely holds coin.
func TestParseBondReadsTermsBack(t *testing.T) {
	_, pubs := memberKeys(t, 1)
	const blocks = MinBondBlocks + 500

	bond, err := BondScript(pubs[0], blocks)
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}
	terms, err := ParseBond(bond)
	if err != nil {
		t.Fatalf("parse bond: %v", err)
	}
	if !bytes.Equal(terms.Owner, pubs[0]) {
		t.Fatalf("owner is %x, want %x", terms.Owner, pubs[0])
	}
	if terms.LockBlocks != blocks {
		t.Fatalf("lock is %d blocks, want %d", terms.LockBlocks, blocks)
	}
}

func TestParseBondRejectsNonBonds(t *testing.T) {
	privs, pubs := memberKeys(t, 2)
	_ = privs

	anyoneCanSpend, err := txscript.NewScriptBuilder().AddOp(txscript.OP_TRUE).Script()
	if err != nil {
		t.Fatalf("build script: %v", err)
	}

	// A script with the shape of a bond but a second way out of it: the
	// timelock and owner read back fine, yet the coin is not locked at all.
	backdoor, err := txscript.NewScriptBuilder().
		AddInt64(int64(MinBondBlocks)).
		AddOp(txscript.OP_CHECKSEQUENCEVERIFY).
		AddOp(txscript.OP_DROP).
		AddData(pubs[0]).
		AddInt64(sigType).
		AddOp(txscript.OP_CHECKSIGALTVERIFY).
		AddOp(txscript.OP_TRUE).
		AddOp(txscript.OP_DROP).
		AddOp(txscript.OP_TRUE).
		Script()
	if err != nil {
		t.Fatalf("build script: %v", err)
	}

	// An escrow redeem script is a real script of ours, and still not a bond.
	notABond, err := RedeemScript(pubs[0], pubs, testCSVBlocks)
	if err != nil {
		t.Fatalf("redeem script: %v", err)
	}

	for name, script := range map[string][]byte{
		"anyone can spend":     anyoneCanSpend,
		"bond with a backdoor": backdoor,
		"an escrow script":     notABond,
		"empty":                nil,
	} {
		if _, err := ParseBond(script); err == nil {
			t.Fatalf("expected %q to be rejected as a bond", name)
		}
	}
}
