package runtime

import (
	"strings"
	"sync"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
)

const bondOutpoint = "aa11bb22aa11bb22aa11bb22aa11bb22aa11bb22aa11bb22aa11bb22aa11bb22:0"

func payTo(t *testing.T) string {
	t.Helper()
	var h [20]byte
	copy(h[:], "a twenty byte hash..")
	addr, err := stdaddrPKH(h[:])
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	return addr
}

func stdaddrPKH(hash []byte) (string, error) {
	addr, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(hash, chaincfg.TestNet3Params())
	if err != nil {
		return "", err
	}
	return addr.String(), nil
}

func TestSplitOutpointRefusesMalformedValues(t *testing.T) {
	for _, bad := range []string{"", "abc", "abc:", ":0", "abc:x", strings.Repeat("a", 63) + ":0", strings.Repeat("a", 65) + ":0"} {
		if _, _, err := splitOutpoint(bad); err == nil {
			t.Errorf("%q was read as an outpoint", bad)
		}
	}
	txid, vout, err := splitOutpoint(bondOutpoint)
	if err != nil || vout != 0 || len(txid) != 64 {
		t.Fatalf("valid outpoint did not parse: %q %d %v", txid, vout, err)
	}
}

type withholding struct {
	battleshipsRules
	mu      sync.Mutex
	against uint32
	named   bool
}

func (w *withholding) hold(seat uint32) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.against, w.named = seat, true
}

func (w *withholding) WillCoSign(_ string, seat uint32) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.named || seat != w.against
}
