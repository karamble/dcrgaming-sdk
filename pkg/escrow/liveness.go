package escrow

import (
	"bytes"
	"fmt"

	"github.com/decred/dcrd/txscript/v4"
)

// ClaimBlocks is how long a player has to prove they are still at the table
// before the others take their bond.
//
// Three blocks, about a quarter of an hour. This is a live game: a seat that has
// not attached its turn to the log for fifteen minutes has left, and the rest of
// the table should not be held there. The window is not sized for somebody
// asleep - it is sized for a reconnect, and anyone who wanted longer wanted a
// correspondence game.
//
// It is also the whole grace period. An owner present inside it answers with
// one signature of their own, the bond comes back, and the game carries on.
const ClaimBlocks uint32 = 3

// TableBondScript builds a bond that a table can take from a player who walks
// out of a hand.
//
//	OP_IF   <every member> 2 OP_CHECKSIGALTVERIFY ...           # alive
//	OP_ELSE <lock> CSV DROP <owner> 2 OP_CHECKSIGALTVERIFY      # backstop
//
// Two ways out, and the branch that is not here is the design.
//
// **Alive** needs every member's signature, the owner included, and has no
// timelock. Every ordinary spend is this branch - the release when a table ends,
// and the accusation when a seat goes quiet - and which of those happens is
// decided by which pre-signed transaction exists, not by the script.
//
// **Backstop** is the owner alone after a long lock, so a table that simply
// dissolves does not strand anybody's coin forever.
//
// There is deliberately no branch the other members can take without the owner.
// Its CSV would count from this output, which confirms before the table deals,
// so the lock would be long satisfied by the time anybody abandoned - the others
// could take the bond at a moment of their choosing, with no window for an
// answer. Instead the bond only moves with the owner's signature among the rest,
// so an accusation has to be a transaction the owner agreed to in advance. The
// owner signs willingly because its one output is ClaimedBondScript: spendable
// by the owner alone at once, and by the accusers only after ClaimBlocks of a
// window that opens when the accusation confirms, because that is the output its
// CSV counts from. CheckAccuseDraft is what holds a pre-signed accusation to
// that output, standing where a covenant would if Decred had them.
//
// # Why abandonment is decided by a window and not by a proof
//
// Nothing can prove a player left. A seat that goes quiet and a seat whose
// messages are being dropped look identical from outside, and heads-up the only
// witness is the person who benefits from the answer. Every design that asks
// somebody to adjudicate hands that somebody a way to rob an honest player.
//
// So it is not adjudicated. Abandonment is *defined* as failing to answer on
// chain inside the window, which is a fact rather than a claim: the accused
// answers to Decred, not to their opponent. The answer needs no signature but
// the owner's, so the ability to answer survives losing everything but the
// seed, and collusion buys nothing - a table full of liars can accuse, and one
// uncensorable answer keeps the bond.
//
// An accusation withheld from the group chat is not therefore hidden. The
// window opens at confirmation, on the chain the accused watches: a spend still
// in the mempool shows as the confirmed view holding an output the mempool view
// says is gone, and one already mined is the window opening, not the window
// missed. What remains is the ordinary requirement that the accused be running
// inside the window, which is what a bond is for.
//
// # Why this is not the forfeiture in forfeit.go
//
// The two punishments need opposite properties and cannot share a branch.
// Equivocation is punished by a branch nobody can contest, because the cheat is
// online by definition and would contest it. Abandonment is punished by a branch
// that *must* be contestable, because the accusation might be false. One is
// keyed to a secret the cheat published themselves; this one is keyed to
// silence.
func TableBondScript(owner []byte, members [][]byte, lockBlocks uint32) ([]byte, error) {
	if err := checkPubKey(owner); err != nil {
		return nil, fmt.Errorf("owner key: %w", err)
	}
	sorted, err := CanonicalMembers(members)
	if err != nil {
		return nil, err
	}
	if !containsKey(sorted, owner) {
		return nil, fmt.Errorf("the bond's owner is not at this table")
	}
	if len(sorted) < 2 {
		return nil, fmt.Errorf("a table bond needs at least 2 members, not %d", len(sorted))
	}
	if lockBlocks < MinBondBlocks {
		return nil, fmt.Errorf("bond lock of %d blocks is under the %d block minimum",
			lockBlocks, MinBondBlocks)
	}
	if lockBlocks > MaxCSVBlocks {
		return nil, fmt.Errorf("bond lock of %d blocks is beyond %d, so it could never be reclaimed",
			lockBlocks, MaxCSVBlocks)
	}
	b := txscript.NewScriptBuilder()
	b.AddOp(txscript.OP_IF)
	for _, m := range sorted {
		b.AddData(m).AddInt64(sigType).AddOp(txscript.OP_CHECKSIGALTVERIFY)
	}
	b.AddOp(txscript.OP_ELSE).
		AddInt64(int64(lockBlocks)).
		AddOp(txscript.OP_CHECKSEQUENCEVERIFY).
		AddOp(txscript.OP_DROP).
		AddData(owner).
		AddInt64(sigType).
		AddOp(txscript.OP_CHECKSIGALTVERIFY).
		AddOp(txscript.OP_ENDIF).
		AddOp(txscript.OP_TRUE)

	script, err := b.Script()
	if err != nil {
		return nil, err
	}
	if len(script) > txscript.MaxScriptElementSize {
		return nil, fmt.Errorf("a table bond for %d members is %d bytes, over the %d byte push limit",
			len(sorted), len(script), txscript.MaxScriptElementSize)
	}
	return script, nil
}

func withoutKey(members [][]byte, key []byte) [][]byte {
	out := make([][]byte, 0, len(members))
	for _, m := range members {
		if !bytes.Equal(m, key) {
			out = append(out, m)
		}
	}
	return out
}

// AliveSigScript spends the branch every pre-agreed spend uses - the release
// when a table ends, and the accusation.
//
// Signatures are in canonical member order - the same order the script checks
// them in, and the same order CanonicalMembers puts them in, so every player
// derives it from the roster rather than being told.
func AliveSigScript(bond []byte, sigs [][]byte) ([]byte, error) {
	terms, err := ParseTableBond(bond)
	if err != nil {
		return nil, err
	}
	if len(sigs) != len(terms.Members) {
		return nil, fmt.Errorf("this branch needs all %d members' signatures, not %d",
			len(terms.Members), len(sigs))
	}
	return branchSigScript(bond, sigs, txscript.OP_1)
}

// BackstopSigScript spends the owner's own way out, once the long lock matures.
func BackstopSigScript(bond, ownerSig []byte) ([]byte, error) {
	if _, err := ParseTableBond(bond); err != nil {
		return nil, err
	}
	return branchSigScript(bond, [][]byte{ownerSig}, txscript.OP_0)
}

// branchSigScript assembles signatures and branch selectors.
//
// Both go on backwards, and for the same reason: the script consumes the top of
// the stack first, so the last thing pushed is the first thing read. The
// selectors are listed here in the order the nested OP_IFs consume them and
// reversed on the way out, because writing them in consumption order is the
// only way this stays readable.
func branchSigScript(bond []byte, sigs [][]byte, selectors ...byte) ([]byte, error) {
	for i, sig := range sigs {
		if len(sig) != SigLen {
			return nil, fmt.Errorf("signature %d is %d bytes, want %d", i, len(sig), SigLen)
		}
	}
	b := txscript.NewScriptBuilder()
	for i := len(sigs) - 1; i >= 0; i-- {
		b.AddData(sigs[i])
	}
	for i := len(selectors) - 1; i >= 0; i-- {
		b.AddOp(selectors[i])
	}
	return b.AddData(bond).Script()
}

// TableBondTerms is what a table bond commits to.
type TableBondTerms struct {
	Owner   []byte
	Members [][]byte // canonical order, owner included
	Others  [][]byte // canonical order, owner excluded
	// ClaimBlocks is the window an accusation opens, carried here for the
	// callers that derive a claimed bond. It is not in the script: the bond
	// has no branch that waits, so there is nothing for it to commit to, and
	// it is the same constant at every table.
	ClaimBlocks uint32
	LockBlocks  uint32
}

// ParseTableBond reads a table bond back.
//
// Every player runs this on every other player's bond before the first hand. A
// bond that does not name this table's exact roster is one no accusation these
// players pre-sign could ever spend - it looks like a bond, holds real coin, and
// could never be taken from anybody.
func ParseTableBond(bond []byte) (*TableBondTerms, error) {
	tokenizer := txscript.MakeScriptTokenizer(scriptVersion, bond)

	var (
		keys     [][]byte
		locks    []uint32
		last     uint32
		haveLast bool
	)
	for tokenizer.Next() {
		switch op := tokenizer.Opcode(); {
		case op == txscript.OP_CHECKSEQUENCEVERIFY:
			if !haveLast {
				return nil, fmt.Errorf("script has a timelock with no length")
			}
			locks = append(locks, last)
		case op == txscript.OP_DATA_33:
			keys = append(keys, append([]byte(nil), tokenizer.Data()...))
		default:
			if n, ok := smallIntOrData(tokenizer); ok {
				last, haveLast = n, true
			}
		}
	}
	if err := tokenizer.Err(); err != nil {
		return nil, fmt.Errorf("parse table bond script: %w", err)
	}
	if len(locks) != 1 {
		return nil, fmt.Errorf("a table bond has one lock; this has %d timelocks", len(locks))
	}
	// Every member, then the owner again for the backstop: n+1 keys.
	if len(keys) < 3 {
		return nil, fmt.Errorf("script names %d keys, which is not a table bond's layout", len(keys))
	}
	owner := keys[len(keys)-1]
	members := keys[:len(keys)-1]

	terms := &TableBondTerms{
		Owner:       owner,
		Members:     members,
		Others:      withoutKey(members, owner),
		ClaimBlocks: ClaimBlocks,
		LockBlocks:  locks[0],
	}

	// Rebuilding is the real check - it is the only thing that rules out a
	// script carrying these same pushes alongside another path to the coin.
	want, err := TableBondScript(terms.Owner, terms.Members, terms.LockBlocks)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(want, bond) {
		return nil, fmt.Errorf("script is not a table bond on the terms it appears to carry")
	}
	return terms, nil
}

// MemberIndex reports where a key sits in a bond's canonical member order, so a
// player can find which signature slot is theirs.
func MemberIndex(terms *TableBondTerms, key []byte) (int, error) {
	if terms == nil {
		return 0, fmt.Errorf("no bond")
	}
	for i, m := range terms.Members {
		if bytes.Equal(m, key) {
			return i, nil
		}
	}
	return 0, fmt.Errorf("that key is not a member of this table bond")
}

// MemberIndexOf reports where a key sits in a given list, for the callers that
// hold one rather than a whole bond.
func MemberIndexOf(keys [][]byte, key []byte) (int, error) {
	for i, m := range keys {
		if bytes.Equal(m, key) {
			return i, nil
		}
	}
	return 0, fmt.Errorf("that key is not one of these")
}
