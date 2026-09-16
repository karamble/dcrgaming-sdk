package escrow

import (
	"bytes"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4"
	"testing"
)

const testCSVBlocks uint32 = 64

// Spending VM tests live in pkg/finance; this package checks public derivation.
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
	for n := 2; n <= MaxMembers; n++ {
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
