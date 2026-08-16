package escrow

import (
	"bytes"
	"testing"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/wire"
)

const (
	testCSVBlocks = 64
	testInAtoms   = 1_000_000
	testOutAtoms  = 990_000
)

// csvFlags enables the relative timelock check; without it
// OP_CHECKSEQUENCEVERIFY degrades to a no-op and the refund tests prove
// nothing.
const csvFlags = txscript.ScriptVerifyCheckSequenceVerify

func memberKeys(t *testing.T, n int) ([]*secp256k1.PrivateKey, [][]byte) {
	t.Helper()
	privs := make([]*secp256k1.PrivateKey, 0, n)
	pubs := make([][]byte, 0, n)
	for range n {
		priv, err := secp256k1.GeneratePrivateKey()
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		privs = append(privs, priv)
		pubs = append(pubs, priv.PubKey().SerializeCompressed())
	}
	return privs, pubs
}

// privFor returns the private key matching a canonical member position.
func privFor(t *testing.T, privs []*secp256k1.PrivateKey, pub []byte) *secp256k1.PrivateKey {
	t.Helper()
	for _, p := range privs {
		if bytes.Equal(p.PubKey().SerializeCompressed(), pub) {
			return p
		}
	}
	t.Fatalf("no private key for member")
	return nil
}

func spendTx(t *testing.T, sequence uint32) *wire.MsgTx {
	t.Helper()
	tx := wire.NewMsgTx()
	// OP_CHECKSEQUENCEVERIFY refuses anything under version 2
	// (wire.TxVersionSeqLock); OP_CHECKSIGALTVERIFY has no version gate.
	tx.Version = 3

	var prev chainhash.Hash
	copy(prev[:], bytes.Repeat([]byte{0x11}, chainhash.HashSize))
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: prev, Index: 0, Tree: wire.TxTreeRegular},
		ValueIn:          testInAtoms,
		Sequence:         sequence,
	})

	payScript, err := txscript.NewScriptBuilder().AddOp(txscript.OP_TRUE).Script()
	if err != nil {
		t.Fatalf("build payout script: %v", err)
	}
	tx.AddTxOut(wire.NewTxOut(testOutAtoms, payScript))
	return tx
}

func signInput(t *testing.T, priv *secp256k1.PrivateKey, redeem []byte, tx *wire.MsgTx) []byte {
	t.Helper()
	sighash, err := txscript.CalcSignatureHash(redeem, txscript.SigHashAll, tx, 0, nil)
	if err != nil {
		t.Fatalf("sighash: %v", err)
	}
	sig, err := schnorr.Sign(priv, sighash)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return append(sig.Serialize(), byte(txscript.SigHashAll))
}

// execute runs the real consensus script engine over the spend, which is the
// only authority on whether these scripts work.
func execute(t *testing.T, redeem []byte, tx *wire.MsgTx, flags txscript.ScriptFlags) error {
	t.Helper()
	_, pkScript, err := Address(redeem, chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	vm, err := txscript.NewEngine(pkScript, tx, 0, flags, scriptVersion, nil)
	if err != nil {
		return err
	}
	return vm.Execute()
}

// settlementSpend builds a fully signed settlement spend for the given members.
func settlementSpend(t *testing.T, privs []*secp256k1.PrivateKey, members [][]byte, redeem []byte) *wire.MsgTx {
	t.Helper()
	tx := spendTx(t, 0)
	sigs := make([][]byte, 0, len(members))
	for _, m := range members {
		sigs = append(sigs, signInput(t, privFor(t, privs, m), redeem, tx))
	}
	sigScript, err := SettlementSigScript(redeem, sigs)
	if err != nil {
		t.Fatalf("settlement sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sigScript
	return tx
}

func TestSettlementBranchAcceptsEveryMember(t *testing.T) {
	privs, pubs := memberKeys(t, 3)
	members, err := CanonicalMembers(pubs)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	redeem, err := RedeemScript(members[0], members, testCSVBlocks)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}

	tx := settlementSpend(t, privs, members, redeem)
	if err := execute(t, redeem, tx, 0); err != nil {
		t.Fatalf("full member set should spend the settlement branch: %v", err)
	}
}

// The point of the whole construction: no proper subset of the table can move
// a depositor's stake, so a losing player cannot sweep mid-hand.
func TestSettlementBranchRejectsPartialMemberSet(t *testing.T) {
	privs, pubs := memberKeys(t, 3)
	members, _ := CanonicalMembers(pubs)
	redeem, err := RedeemScript(members[0], members, testCSVBlocks)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}

	for drop := range members {
		tx := spendTx(t, 0)
		b := txscript.NewScriptBuilder()
		// Push every signature except one, still in reverse order.
		for i := len(members) - 1; i >= 0; i-- {
			if i == drop {
				continue
			}
			b.AddData(signInput(t, privFor(t, privs, members[i]), redeem, tx))
		}
		b.AddOp(txscript.OP_1).AddData(redeem)
		sigScript, err := b.Script()
		if err != nil {
			t.Fatalf("build sigscript: %v", err)
		}
		tx.TxIn[0].SignatureScript = sigScript

		if err := execute(t, redeem, tx, 0); err == nil {
			t.Fatalf("settlement branch accepted a set missing member %d", drop)
		}
	}
}

// The specific defect this construction closes: the owner alone must not be
// able to spend the settlement branch of their own escrow.
func TestSettlementBranchRejectsOwnerAlone(t *testing.T) {
	privs, pubs := memberKeys(t, 3)
	members, _ := CanonicalMembers(pubs)
	owner := members[0]
	redeem, err := RedeemScript(owner, members, testCSVBlocks)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}

	tx := spendTx(t, 0)
	ownerSig := signInput(t, privFor(t, privs, owner), redeem, tx)
	sigScript, err := txscript.NewScriptBuilder().
		AddData(ownerSig).
		AddOp(txscript.OP_1).
		AddData(redeem).
		Script()
	if err != nil {
		t.Fatalf("build sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sigScript

	if err := execute(t, redeem, tx, 0); err == nil {
		t.Fatalf("owner alone spent the settlement branch")
	}
}

// A correctly shaped spend whose signatures are not all from the table must
// still fail. This separates real signature verification from stack shape: the
// partial-set test above could in principle fail on depth alone.
func TestSettlementBranchRejectsForgedSignature(t *testing.T) {
	privs, pubs := memberKeys(t, 3)
	outsiderPrivs, _ := memberKeys(t, 1)
	members, _ := CanonicalMembers(pubs)
	redeem, err := RedeemScript(members[0], members, testCSVBlocks)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}

	for forge := range members {
		tx := spendTx(t, 0)
		sigs := make([][]byte, 0, len(members))
		for i, m := range members {
			signer := privFor(t, privs, m)
			if i == forge {
				signer = outsiderPrivs[0]
			}
			sigs = append(sigs, signInput(t, signer, redeem, tx))
		}
		sigScript, err := SettlementSigScript(redeem, sigs)
		if err != nil {
			t.Fatalf("settlement sigscript: %v", err)
		}
		tx.TxIn[0].SignatureScript = sigScript

		if err := execute(t, redeem, tx, 0); err == nil {
			t.Fatalf("settlement branch accepted a forged signature at %d", forge)
		}
	}
}

// Signatures are consumed top of stack first, so their push order is the
// reverse of the script's check order. Getting it backwards must not validate.
func TestSettlementSigScriptOrderMatters(t *testing.T) {
	privs, pubs := memberKeys(t, 3)
	members, _ := CanonicalMembers(pubs)
	redeem, err := RedeemScript(members[0], members, testCSVBlocks)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}

	tx := spendTx(t, 0)
	sigs := make([][]byte, 0, len(members))
	for _, m := range members {
		sigs = append(sigs, signInput(t, privFor(t, privs, m), redeem, tx))
	}

	reversed := make([][]byte, len(sigs))
	for i, s := range sigs {
		reversed[len(sigs)-1-i] = s
	}
	sigScript, err := SettlementSigScript(redeem, reversed)
	if err != nil {
		t.Fatalf("settlement sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sigScript

	if err := execute(t, redeem, tx, 0); err == nil {
		t.Fatalf("settlement branch accepted signatures in the wrong order")
	}
}

func TestRefundBranchIsUnilateral(t *testing.T) {
	privs, pubs := memberKeys(t, 3)
	members, _ := CanonicalMembers(pubs)
	owner := members[1]
	redeem, err := RedeemScript(owner, members, testCSVBlocks)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}

	tx := spendTx(t, testCSVBlocks)
	sigScript, err := RefundSigScript(redeem, signInput(t, privFor(t, privs, owner), redeem, tx))
	if err != nil {
		t.Fatalf("refund sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sigScript

	if err := execute(t, redeem, tx, csvFlags); err != nil {
		t.Fatalf("owner should refund alone once CSV matured: %v", err)
	}
}

func TestRefundBranchRequiresMaturedSequence(t *testing.T) {
	privs, pubs := memberKeys(t, 3)
	members, _ := CanonicalMembers(pubs)
	owner := members[1]
	redeem, err := RedeemScript(owner, members, testCSVBlocks)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}

	tx := spendTx(t, testCSVBlocks-1)
	sigScript, err := RefundSigScript(redeem, signInput(t, privFor(t, privs, owner), redeem, tx))
	if err != nil {
		t.Fatalf("refund sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sigScript

	if err := execute(t, redeem, tx, csvFlags); err == nil {
		t.Fatalf("refund branch spent before CSV matured")
	}
}

func TestRefundBranchRejectsNonOwner(t *testing.T) {
	privs, pubs := memberKeys(t, 3)
	members, _ := CanonicalMembers(pubs)
	owner, other := members[1], members[2]
	redeem, err := RedeemScript(owner, members, testCSVBlocks)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}

	tx := spendTx(t, testCSVBlocks)
	sigScript, err := RefundSigScript(redeem, signInput(t, privFor(t, privs, other), redeem, tx))
	if err != nil {
		t.Fatalf("refund sigscript: %v", err)
	}
	tx.TxIn[0].SignatureScript = sigScript

	if err := execute(t, redeem, tx, csvFlags); err == nil {
		t.Fatalf("refund branch accepted a signature from another member")
	}
}

func TestCanonicalMembersIsOrderIndependent(t *testing.T) {
	_, pubs := memberKeys(t, 4)
	forward, err := CanonicalMembers(pubs)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}

	shuffled := [][]byte{pubs[2], pubs[0], pubs[3], pubs[1]}
	reverse, err := CanonicalMembers(shuffled)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	for i := range forward {
		if !bytes.Equal(forward[i], reverse[i]) {
			t.Fatalf("canonical order depends on input order at %d", i)
		}
	}
	for i := 1; i < len(forward); i++ {
		if bytes.Compare(forward[i-1], forward[i]) >= 0 {
			t.Fatalf("members are not ascending at %d", i)
		}
	}
}

func TestCanonicalMembersRejectsBadSets(t *testing.T) {
	_, pubs := memberKeys(t, 2)

	if _, err := CanonicalMembers(nil); err == nil {
		t.Fatalf("expected rejection of an empty member set")
	}
	if _, err := CanonicalMembers([][]byte{pubs[0], pubs[0]}); err == nil {
		t.Fatalf("expected rejection of duplicate members")
	}
	if _, err := CanonicalMembers([][]byte{pubs[0], {0x02, 0x03}}); err == nil {
		t.Fatalf("expected rejection of a malformed key")
	}

	_, many := memberKeys(t, MaxMembers+1)
	if _, err := CanonicalMembers(many); err == nil {
		t.Fatalf("expected rejection of more than %d members", MaxMembers)
	}
}

func TestRedeemScriptRejectsNonMemberOwner(t *testing.T) {
	_, pubs := memberKeys(t, 3)
	_, outsider := memberKeys(t, 1)

	if _, err := RedeemScript(outsider[0], pubs, testCSVBlocks); err == nil {
		t.Fatalf("expected rejection of an owner outside the table")
	}
	if _, err := RedeemScript(pubs[0], pubs, 0); err == nil {
		t.Fatalf("expected rejection of a zero CSV timeout")
	}
}

func TestMemberCountMatchesTable(t *testing.T) {
	for n := 1; n <= MaxMembers; n++ {
		_, pubs := memberKeys(t, n)
		members, _ := CanonicalMembers(pubs)
		redeem, err := RedeemScript(members[0], members, testCSVBlocks)
		if err != nil {
			t.Fatalf("redeem for %d members: %v", n, err)
		}
		got, err := MemberCount(redeem)
		if err != nil {
			t.Fatalf("member count: %v", err)
		}
		if got != n {
			t.Fatalf("member count = %d, want %d", got, n)
		}
	}
}

// The deposit address commits to the whole roster, so a table cannot change
// membership without every player re-funding. Locking this in as a test because
// it drives the escrow lifecycle, not just the script.
func TestDepositAddressDependsOnRoster(t *testing.T) {
	_, pubs := memberKeys(t, 3)
	_, replacement := memberKeys(t, 1)

	members, _ := CanonicalMembers(pubs)
	redeem, err := RedeemScript(members[0], members, testCSVBlocks)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	owner := members[0]

	swapped, _ := CanonicalMembers([][]byte{owner, members[1], replacement[0]})
	otherRedeem, err := RedeemScript(owner, swapped, testCSVBlocks)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}

	addr, _, err := Address(redeem, chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	otherAddr, _, err := Address(otherRedeem, chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	if addr.String() == otherAddr.String() {
		t.Fatalf("deposit address did not change when the roster changed")
	}
}

// Members reads the roster back out of a script so a spend is assembled against
// the script it actually has to satisfy.
func TestMembersReportsScriptOrder(t *testing.T) {
	privs, pubs := memberKeys(t, 4)
	_ = privs

	redeem, err := RedeemScript(pubs[0], pubs, testCSVBlocks)
	if err != nil {
		t.Fatalf("redeem script: %v", err)
	}

	got, err := Members(redeem)
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	want, err := CanonicalMembers(pubs)
	if err != nil {
		t.Fatalf("canonical members: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d members, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("member %d is %x, want %x", i, got[i], want[i])
		}
	}

	// The owner's key appears again in the refund branch; reading must stop at
	// OP_ELSE or it would report a member twice and demand an extra signature.
	n, err := MemberCount(redeem)
	if err != nil {
		t.Fatalf("member count: %v", err)
	}
	if len(got) != n {
		t.Fatalf("Members returned %d keys but MemberCount says %d", len(got), n)
	}
}

func TestMembersRejectsNonRosterScripts(t *testing.T) {
	trivial, err := txscript.NewScriptBuilder().AddOp(txscript.OP_TRUE).Script()
	if err != nil {
		t.Fatalf("build script: %v", err)
	}
	if _, err := Members(trivial); err == nil {
		t.Fatalf("expected a script with no settlement branch to be rejected")
	}
	if _, err := Members(nil); err == nil {
		t.Fatalf("expected an empty script to be rejected")
	}
}
