package runtime

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

// seatBondKeyFor derives a table-scoped identity key for membership proofs.
// It cannot spend the bond; the bridge owns the separate recovery key.
func (r *Runtime) seatBondKeyFor(sid string) (*secp256k1.PrivateKey, error) {
	key, err := r.identity.DeriveKey(r.seatTags.Bond, sid)
	if err != nil {
		return nil, fmt.Errorf("derive this seat's bond key: %w", err)
	}
	return key, nil
}

// seatBondLock is how long a seat bond sits behind its timelock.
// seatBondScript is the script this seat's bond sits behind.
func (r *Runtime) seatBondScript(terms membership.Terms) ([]byte, error) {
	r.mu.Lock()
	pub := ""
	if t := r.tables[terms.SID]; t != nil {
		pub = t.bridgeKey
	}
	r.mu.Unlock()
	return r.bondScriptFor(terms, pub)
}
func (r *Runtime) bondScriptFor(terms membership.Terms, pub string) ([]byte, error) {
	key, err := r.seatBondKeyFor(terms.SID)
	if err != nil {
		return nil, err
	}
	if pub == "" {
		return nil, fmt.Errorf("bridge-controlled spending key is required")
	}
	recovery, err := hex.DecodeString(pub)
	if err != nil {
		return nil, err
	}
	return escrow.BridgeBondScript(key.PubKey().SerializeCompressed(), recovery, terms.BondLockBlocks)
}

// FundSeatBond requests the table's explicitly agreed admission deposit.
// Only the bridge can approve, sign, and publish its funding transaction.
func (r *Runtime) FundSeatBond(ctx context.Context, match string) error {
	t, err := r.rawTable(match)
	if err != nil {
		return err
	}
	r.mu.Lock()
	already := t.seatBond
	r.mu.Unlock()
	if already.outpoint != "" {
		return nil
	}

	terms := t.terms
	tip, err := r.bridge.ChainTip(ctx)
	if err != nil {
		return err
	}
	existing := false
	for _, rec := range r.book.All() {
		if rec.Match == match && rec.Purpose == "seatbond" {
			existing = true
		}
	}
	if tip.Height > int64(terms.Until) && !existing {
		r.mu.Lock()
		t.recoveryOnly = true
		t.recoveryReason = "admission expired"
		r.mu.Unlock()
		if err := r.keep(t); err != nil {
			return err
		}
		return fmt.Errorf("admission deadline passed")
	}
	script, err := r.seatBondScript(terms)
	if err != nil {
		return err
	}
	pkScript, addr, err := membership.PkScriptAndAddr(script, r.params)
	if err != nil {
		return err
	}
	atoms := int64(terms.BondAtoms)
	if atoms <= 0 {
		return fmt.Errorf("explicit admission bond amount is required")
	}
	rec, err := r.askFor(ctx, spend.Record{
		Match: match, Purpose: "seatbond",
		Address: addr, Atoms: atoms, PkScript: pkScript,
	})
	if err != nil {
		return err
	}
	out, err := r.findOutput(ctx, rec)
	if err != nil {
		return err
	}
	r.mu.Lock()
	t.seatBond = out
	r.mu.Unlock()
	return r.keep(t)
}

// seatBondOutpoint is the admission deposit funded for this table.
func (r *Runtime) seatBondOutpoint(t *table) (string, error) {
	r.mu.Lock()
	out := t.seatBond.outpoint
	r.mu.Unlock()
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("this table's seat bond is not on the chain yet")
	}
	return out, nil
}
