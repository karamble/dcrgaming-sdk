package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

// What a seat stakes to be allowed to join at all, and how many of them.
//
// A join binds to a bond, which is what stops a stranger filling a table's
// seats for nothing. How many bonds a player needs is not a fact about the
// protocol - it is an economic choice, and the two existing games made
// different ones. Neither is wrong, so the SDK states the choice rather than
// picking one and calling the other a special case.

// BondScope says how many seat bonds a player posts.
type BondScope int

const (
	// BondPerIdentity is one bond backing every table this player sits at.
	// Cheap for somebody playing several at once, and the sybil cost is
	// paid once per identity. dcrpoker has always worked this way.
	BondPerIdentity BondScope = iota

	// BondPerTable is a fresh bond for each table. Dearer - a player at
	// three tables locks three bonds - and stronger for it: one bond cannot
	// stand behind two seats, so a player cannot be at more tables than
	// they have staked for. dcrbattleships works this way.
	BondPerTable
)

func (s BondScope) String() string {
	if s == BondPerTable {
		return "one bond per table"
	}
	return "one bond per identity"
}

// seatBondKeyFor derives the key that opens this seat's bond.
//
// Per identity the session id is left out, and that is not an oversight:
// [identity.Identity.BondDeposit] is one outpoint per identity, so the key
// that opens it has to be one key too. Per table it is included, because each
// table's bond is its own coin behind its own script.
func (r *Runtime) seatBondKeyFor(sid string) (*secp256k1.PrivateKey, error) {
	scope := ""
	if r.bondScope == BondPerTable {
		scope = sid
	}
	key, err := r.identity.DeriveKey(r.seatTags.Bond, scope)
	if err != nil {
		return nil, fmt.Errorf("derive this seat's bond key: %w", err)
	}
	return key, nil
}

// seatBondLock is how long a seat bond sits behind its timelock.
func seatBondLock(terms membership.Terms) uint32 {
	if terms.BondLockBlocks > 0 {
		return terms.BondLockBlocks
	}
	// A table that states no bond terms uses the escrow floor, which is
	// what dcrpoker has always done.
	return escrow.MinBondBlocks
}

// seatBondScript is the script this seat's bond sits behind.
func (r *Runtime) seatBondScript(terms membership.Terms) ([]byte, error) {
	key, err := r.seatBondKeyFor(terms.SID)
	if err != nil {
		return nil, err
	}
	script, err := escrow.BondScript(key.PubKey().SerializeCompressed(), seatBondLock(terms))
	if err != nil {
		return nil, fmt.Errorf("build this seat's bond script: %w", err)
	}
	return script, nil
}

// FundSeatBond pays the bond this seat's join will bind to.
//
// Only for a game that posts one per table; per identity there is a single
// deposit and funding it is not a table's business. Paid before the table
// forms, because the join names the bond and a join naming a bond nobody paid
// is a seat that cost nothing.
func (r *Runtime) FundSeatBond(ctx context.Context, match string) error {
	if r.bondScope != BondPerTable {
		return fmt.Errorf("this game posts %s, so a table does not fund one", r.bondScope)
	}
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
		atoms = int64(escrow.MinBondAtoms)
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
	r.keep(t)
	return nil
}

// seatBondOutpoint is where this seat's bond is, whichever way the game counts
// them.
func (r *Runtime) seatBondOutpoint(t *table) (string, error) {
	if r.bondScope == BondPerTable {
		r.mu.Lock()
		out := t.seatBond.outpoint
		r.mu.Unlock()
		if strings.TrimSpace(out) == "" {
			return "", fmt.Errorf(
				"this table's seat bond is not on the chain yet, so there is nothing to join with")
		}
		return out, nil
	}
	out := r.identity.BondDeposit()
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf(
			"this seat has no bond deposit yet, so it has nothing to stake against its word; " +
				"fund one before joining a table")
	}
	return out, nil
}
