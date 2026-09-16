package escrow

import (
	"bytes"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4"
	"testing"
)

func testBondScript(identity []byte, blocks uint32) ([]byte, error) {
	recovery := secp256k1.PrivKeyFromBytes([]byte{123}).PubKey().SerializeCompressed()
	return BridgeBondScript(identity, recovery, blocks)
}
func TestParseBondReadsSeparateAuthorities(t *testing.T) {
	_, pubs := memberKeys(t, 2)
	bond, err := BridgeBondScript(pubs[0], pubs[1], 2016)
	if err != nil {
		t.Fatal(err)
	}
	terms, err := ParseBond(bond)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(terms.Owner, pubs[0]) || !bytes.Equal(terms.Recovery, pubs[1]) || terms.LockBlocks != 2016 {
		t.Fatal("bond lost an authority or delay")
	}
	for _, invalid := range [][]byte{nil, {txscript.OP_TRUE}, append(append([]byte(nil), bond...), txscript.OP_TRUE), bond[:len(bond)-1]} {
		if _, err := ParseBond(invalid); err == nil {
			t.Fatal("noncanonical bond accepted")
		}
	}
	if _, err := BridgeBondScript(pubs[0], pubs[0], 2016); err == nil {
		t.Fatal("game identity accepted as spending authority")
	}
}
