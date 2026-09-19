package runtime

// Small lookups shared by the seating, announce and settlement paths: which
// seat is ours at a table, how an outpoint splits, and how a destination
// address becomes a payment script.

import (
	"fmt"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"strconv"
	"strings"
)

func (r *Runtime) ourSeatAt(sid string) (*table, uint32, error) {
	sid = strings.ToLower(strings.TrimSpace(sid))
	r.mu.Lock()
	t, ok := r.tables[sid]
	r.mu.Unlock()
	if !ok || t.formation() == nil {
		return nil, 0, fmt.Errorf("this game is not at table %q", sid)
	}
	seat, ok := t.formation().OurSeat()
	if !ok {
		return nil, 0, fmt.Errorf("table %q has not seated us yet", sid)
	}
	return t, seat, nil
}
func splitOutpoint(s string) (string, uint32, error) {
	txid, idx, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return "", 0, fmt.Errorf("%q is not an outpoint", s)
	}
	vout, err := strconv.ParseUint(idx, 10, 32)
	if err != nil {
		return "", 0, fmt.Errorf("%q has no output index", s)
	}
	if len(txid) != 64 {
		return "", 0, fmt.Errorf("%q does not name a transaction", s)
	}
	return txid, uint32(vout), nil
}
func payScriptFor(addr string, params stdaddr.AddressParams) ([]byte, error) {
	a, err := stdaddr.DecodeAddress(strings.TrimSpace(addr), params)
	if err != nil {
		return nil, fmt.Errorf("the destination is not an address on this chain: %w", err)
	}
	_, script := a.PaymentScript()
	return script, nil
}
