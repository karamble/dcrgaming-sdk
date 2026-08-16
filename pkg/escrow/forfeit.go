package escrow

import (
	"bytes"
	"fmt"

	"github.com/decred/dcrd/txscript/v4"
)

// MaxForfeitKeys bounds how many opponents one bond can be forfeitable to.
//
// The limit is the 520-byte push that a P2SH redeem script has to fit inside;
// each branch costs 39 bytes and the owner's costs about 42, so the real
// ceiling is around twelve. This sits under it with room, and no table this
// protocol is designed for comes close.
const MaxForfeitKeys = 8

// ForfeitableBondScript builds a bond its owner can reclaim after a timelock,
// and that any one named opponent can take immediately if the owner ever
// equivocates.
//
//	OP_IF   <forfeit_0> 2 OP_CHECKSIGALTVERIFY
//	OP_ELSE OP_IF <forfeit_1> 2 OP_CHECKSIGALTVERIFY
//	OP_ELSE ...
//	        <lockBlocks> OP_CHECKSEQUENCEVERIFY OP_DROP <owner> 2 OP_CHECKSIGALTVERIFY
//	OP_ENDIF ... OP_ENDIF
//	OP_TRUE
//
// The script does not judge anything, and that is the design rather than a
// limitation. Decred script can verify a signature over the spending
// transaction and nothing else - it cannot read a log, compare two entries, or
// evaluate a claim of cheating. Every scheme that needs the chain to decide who
// was wronged dies here.
//
// So the deciding happens off-chain, in arithmetic, before anything is
// broadcast. Each forfeit key weights the owner's log key and one opponent's
// punishment key by coefficients derived from both keys and from the branch
// they belong to - a plain sum would let the opponent announce a key that
// cancels the owner's half, and the reason is written down at
// pkg/forfeit.ForfeitKey. Neither party can produce a signature for the result
// alone: the owner is missing the opponent's half, and the opponent is missing
// the owner's. The owner's half becomes public exactly when they sign two
// different things at one position in the log - which is what equivocation *is*
// - and at that moment the opponent holds both halves.
//
// The result is a punishment nobody adjudicates: no quorum to collude and no
// proof to evaluate. What it does not establish is that a bond is safe. This
// builder is handed a branch set and takes it as given, so an owner that states
// its own script can name one more branch keyed to a point it alone holds, and
// every peer's ForfeitIndex will still find its own branch and say yes.
// Punishment branches carry no timelock, so that extra branch reclaims the bond
// the block after it confirms. Rebuilding the branch set from the settled
// roster is a precondition of using any of this, not an improvement on it.
//
// The timelock has to outlast the window in which cheating could still be
// discovered, or an owner could equivocate and then simply reclaim the bond
// before anyone spent it.
func ForfeitableBondScript(owner []byte, forfeit [][]byte, lockBlocks uint32) ([]byte, error) {
	if err := checkPubKey(owner); err != nil {
		return nil, fmt.Errorf("owner key: %w", err)
	}
	if len(forfeit) == 0 {
		return nil, fmt.Errorf("a forfeitable bond with nobody to forfeit to is just a bond")
	}
	if len(forfeit) > MaxForfeitKeys {
		return nil, fmt.Errorf("a bond can name at most %d opponents, not %d",
			MaxForfeitKeys, len(forfeit))
	}
	seen := make(map[string]bool, len(forfeit))
	for i, f := range forfeit {
		if err := checkPubKey(f); err != nil {
			return nil, fmt.Errorf("forfeit key %d: %w", i, err)
		}
		if bytes.Equal(f, owner) {
			return nil, fmt.Errorf("forfeit key %d is the owner's own key, so the lock means nothing", i)
		}
		if seen[string(f)] {
			return nil, fmt.Errorf("forfeit key %d is named twice", i)
		}
		seen[string(f)] = true
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
	for _, f := range forfeit {
		b.AddOp(txscript.OP_IF).
			AddData(f).
			AddInt64(sigType).
			AddOp(txscript.OP_CHECKSIGALTVERIFY).
			AddOp(txscript.OP_ELSE)
	}
	b.AddInt64(int64(lockBlocks)).
		AddOp(txscript.OP_CHECKSEQUENCEVERIFY).
		AddOp(txscript.OP_DROP).
		AddData(owner).
		AddInt64(sigType).
		AddOp(txscript.OP_CHECKSIGALTVERIFY)
	for range forfeit {
		b.AddOp(txscript.OP_ENDIF)
	}
	b.AddOp(txscript.OP_TRUE)

	script, err := b.Script()
	if err != nil {
		return nil, err
	}
	if len(script) > txscript.MaxScriptElementSize {
		return nil, fmt.Errorf("forfeitable bond script is %d bytes, over the %d byte push limit",
			len(script), txscript.MaxScriptElementSize)
	}
	return script, nil
}

// ForfeitSigScript spends a bond through the punishment branch belonging to one
// opponent.
//
// The branch is selected by the booleans the nested OP_IFs consume, and they
// are consumed in the reverse of the order they were pushed - so taking branch
// i means pushing a true and then i falses. Getting this backwards produces a
// script that fails for reasons that look nothing like the cause, which is why
// the count comes from parsing the bond rather than from the caller.
func ForfeitSigScript(bond, sig []byte, index int) ([]byte, error) {
	terms, err := ParseForfeitableBond(bond)
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(terms.Forfeit) {
		return nil, fmt.Errorf("this bond has %d punishment branches, not one at %d",
			len(terms.Forfeit), index)
	}
	if len(sig) != SigLen {
		return nil, fmt.Errorf("signature is %d bytes, want %d", len(sig), SigLen)
	}
	b := txscript.NewScriptBuilder().
		AddData(sig).
		AddOp(txscript.OP_TRUE)
	for range index {
		b.AddOp(txscript.OP_0)
	}
	return b.AddData(bond).Script()
}

// ForfeitableRefundSigScript reclaims a bond the honest way, once its lock has
// matured. Every punishment branch is declined in turn.
func ForfeitableRefundSigScript(bond, ownerSig []byte) ([]byte, error) {
	terms, err := ParseForfeitableBond(bond)
	if err != nil {
		return nil, err
	}
	if len(ownerSig) != SigLen {
		return nil, fmt.Errorf("signature is %d bytes, want %d", len(ownerSig), SigLen)
	}
	b := txscript.NewScriptBuilder().AddData(ownerSig)
	for range terms.Forfeit {
		b.AddOp(txscript.OP_0)
	}
	return b.AddData(bond).Script()
}

// ForfeitableBondTerms is what a forfeitable bond script commits to.
type ForfeitableBondTerms struct {
	Owner      []byte
	Forfeit    [][]byte
	LockBlocks uint32
}

// ParseForfeitableBond reads a forfeitable bond back.
//
// Every peer must run this on every other peer's bond before playing. A bond
// that does not name *this* player's forfeit key is a bond they can never take,
// and it is indistinguishable from a real one by looking at the amount, the
// confirmations, or anything else on chain. The check that matters is not that
// coin is locked up; it is whose key can take it.
func ParseForfeitableBond(bond []byte) (*ForfeitableBondTerms, error) {
	tokenizer := txscript.MakeScriptTokenizer(scriptVersion, bond)

	var (
		keys     [][]byte
		lock     uint32
		last     uint32
		haveLast bool
		haveCSV  bool
	)
	for tokenizer.Next() {
		op := tokenizer.Opcode()
		switch {
		case op == txscript.OP_CHECKSEQUENCEVERIFY:
			if !haveLast {
				return nil, fmt.Errorf("script has a timelock with no length")
			}
			lock = last
			haveCSV = true
		case op == txscript.OP_DATA_33:
			keys = append(keys, append([]byte(nil), tokenizer.Data()...))
		default:
			if n, ok := smallIntOrData(tokenizer); ok {
				last, haveLast = n, true
			}
		}
	}
	if err := tokenizer.Err(); err != nil {
		return nil, fmt.Errorf("parse forfeitable bond script: %w", err)
	}
	if !haveCSV {
		return nil, fmt.Errorf("script has no timelock; the owner could never reclaim it")
	}
	if len(keys) < 2 {
		return nil, fmt.Errorf("script names %d keys; a forfeitable bond needs an owner and at least one opponent",
			len(keys))
	}

	terms := &ForfeitableBondTerms{
		Owner:      keys[len(keys)-1],
		Forfeit:    keys[:len(keys)-1],
		LockBlocks: lock,
	}

	// Rebuilding is the real check. Reading opcode by opcode would accept a
	// script that happens to contain these pushes alongside another path to
	// the coin; only an exact match rules that out.
	want, err := ForfeitableBondScript(terms.Owner, terms.Forfeit, terms.LockBlocks)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(want, bond) {
		return nil, fmt.Errorf("script is not a forfeitable bond on the terms it appears to carry")
	}
	return terms, nil
}

// ForfeitIndex reports which punishment branch pays to a given key, so a player
// can find their own branch in somebody else's bond.
//
// Returns an error rather than a sentinel when the key is absent, because "your
// key is not in this bond" is the finding that must stop a game from starting,
// and it is too important to be signalled by a -1 somebody forgets to check.
//
// Finding your own branch is not the same as the bond being sound. Nothing here
// relates the branch set to the roster, so a bond can carry an extra branch its
// owner can spend at once alongside the honest ones and still answer this
// question correctly. Rebuild the set from the roster before trusting a bond
// you did not derive yourself.
func ForfeitIndex(terms *ForfeitableBondTerms, key []byte) (int, error) {
	if terms == nil {
		return 0, fmt.Errorf("no bond")
	}
	for i, f := range terms.Forfeit {
		if bytes.Equal(f, key) {
			return i, nil
		}
	}
	return 0, fmt.Errorf("this bond has no punishment branch for that key, so it could never be forfeited to it")
}
