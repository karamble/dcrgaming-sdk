package escrow

import (
	"fmt"

	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
)

// MinBondBlocks is the shortest lock a bond may carry. A bond deters by being
// expensive to hold, so a lock short enough to roll over between tables deters
// nothing. At Decred's ~5 minute blocks this is about a week.
const MinBondBlocks uint32 = 2016

// MinBondAtoms is the smallest bond that counts. It is deliberately well under
// a typical buy-in: a bond is a standing cost of holding an identity, not a
// per-table stake.
//
// It lives here beside the script because both a referee and a peer have to
// reach the same verdict about the same deposit, and a threshold each of them
// kept its own copy of is one they would eventually disagree on. Which is also
// why changing it means changing it everywhere at once: a peer running the old
// number refuses a bond a peer running the new one accepts, and neither is
// wrong.
//
// Charged twice - once to hold an identity at all, and again for every table
// joined - so a day's testing costs this times the number of tables, each
// locked for a week.
//
// The floor is set by what a bond has to fund rather than by taste. A dispute
// costs the accused bond two fees a round, and what has to survive the last
// round is a share for every seat that could collect on it, so the smallest
// workable bond at a table of n seats is (n-1) * MinShareAtoms + 2 * claimFee -
// at the six seats escrow.MaxMembers allows, 120,001 atoms for a single
// accusation and 260,001 for the full AccuseDepth. Below that a table deals with
// a bond nothing can be claimed against, and nothing stops it dealing.
//
// This sits about four times over the six seat figure, which puts every table
// size in the regime where AccuseDepth bounds the ladder rather than the
// arithmetic does - so how many rounds a dispute can run stops depending on the
// bond at all. See escrow.AffordableDepth.
const MinBondAtoms uint64 = 1_000_000 // 0.01 DCR

// BondConfirmations is what a bond must have before it counts. A deposit that
// is not yet buried can still be replaced, and a bond that can be undone after
// it was accepted is not a cost.
const BondConfirmations uint32 = 2

// StakeConfirmations is what a seat's stake must have before the rest of the
// table will play against it.
//
// Fewer than a bond wants, deliberately. A bond is posted once and lasts a
// week, so waiting two blocks for one costs nothing anybody notices; a stake is
// paid at the start of every table, and each block of waiting is five minutes
// with everybody sitting watching. One confirmation still means the payment is
// in a block rather than merely in a mempool, which is the difference that
// matters - it can no longer be replaced by fee, only undone by a
// reorganisation.
//
// This is each peer's own policy rather than something the table agrees. A peer
// that wanted more would simply admit a stake later than its neighbours, which
// costs it time and costs the protocol nothing.
const StakeConfirmations uint32 = 1

// BondScript builds a fidelity bond: a deposit its owner can reclaim only after
// a relative timelock, and that nobody else can ever spend.
//
//	<lockBlocks> OP_CHECKSEQUENCEVERIFY OP_DROP <owner> 2 OP_CHECKSIGALTVERIFY OP_TRUE
//
// The bond is posted at registration, before a player takes a seat, and it is
// deliberately not forfeitable. That is a consequence of when it has to exist
// rather than a softening of the penalty: a bond forfeitable to the other
// players would have to name them in its script, and at registration there is
// no roster yet to name - that is the whole reason funding is roster-first. The
// only party known that early is the referee, and a bond forfeitable to the
// referee would hand it custody of exactly the funds this design takes away
// from it.
//
// So the cost is the lock-up, not a transfer. What it buys:
//
//   - Sybil resistance. zkidentity keys are free to generate, so anything keyed
//     to identity alone is worth nothing; a fresh identity needs a fresh bond
//     and a fresh week of locked coin.
//   - Something expensive for reputation to attach to, which is what makes a
//     record of past behaviour worth keeping at all.
//   - A standing deterrent against register-and-vanish, which roster-first
//     funding otherwise makes cheap: a seat held by someone who never funds
//     keeps every other stake waiting on its CSV.
//
// It does not compensate anyone for that wait. Compensation needs a forfeitable
// bond, which can only be posted once the roster is closed - alongside the
// stake, not at registration.
func BondScript(owner []byte, lockBlocks uint32) ([]byte, error) {
	if err := checkPubKey(owner); err != nil {
		return nil, fmt.Errorf("owner key: %w", err)
	}
	if lockBlocks < MinBondBlocks {
		return nil, fmt.Errorf("bond lock of %d blocks is under the %d block minimum",
			lockBlocks, MinBondBlocks)
	}
	if lockBlocks > MaxCSVBlocks {
		return nil, fmt.Errorf("bond lock of %d blocks is beyond %d, so it could never be reclaimed",
			lockBlocks, MaxCSVBlocks)
	}

	b := txscript.NewScriptBuilder().
		AddInt64(int64(lockBlocks)).
		AddOp(txscript.OP_CHECKSEQUENCEVERIFY).
		AddOp(txscript.OP_DROP).
		AddData(owner).
		AddInt64(sigType).
		AddOp(txscript.OP_CHECKSIGALTVERIFY).
		AddOp(txscript.OP_TRUE)

	script, err := b.Script()
	if err != nil {
		return nil, err
	}
	if len(script) > txscript.MaxScriptElementSize {
		return nil, fmt.Errorf("bond script is %d bytes, over the %d byte push limit",
			len(script), txscript.MaxScriptElementSize)
	}
	return script, nil
}

// BondSigScript builds the signature script that reclaims a bond once its lock
// has matured. The spending input's sequence must satisfy the timelock.
func BondSigScript(bond, ownerSig []byte) ([]byte, error) {
	if len(ownerSig) != SigLen {
		return nil, fmt.Errorf("signature is %d bytes, want %d", len(ownerSig), SigLen)
	}
	return txscript.NewScriptBuilder().
		AddData(ownerSig).
		AddData(bond).
		Script()
}

// BondTerms is what a bond script commits to.
type BondTerms struct {
	Owner      []byte
	LockBlocks uint32
}

// ParseBond reads a bond script back, so a referee can check that a deposit a
// player points at really is a bond of theirs on the terms claimed rather than
// some other script that happens to hold coin.
func ParseBond(bond []byte) (*BondTerms, error) {
	tokenizer := txscript.MakeScriptTokenizer(scriptVersion, bond)

	var (
		terms   BondTerms
		ops     []byte
		haveCSV bool
	)
	for tokenizer.Next() {
		op := tokenizer.Opcode()
		ops = append(ops, op)
		switch {
		case op == txscript.OP_CHECKSEQUENCEVERIFY:
			// The lock is whatever was pushed immediately before.
			haveCSV = true
		case op == txscript.OP_DATA_33:
			terms.Owner = append([]byte(nil), tokenizer.Data()...)
		case !haveCSV && terms.LockBlocks == 0:
			if n, ok := smallIntOrData(tokenizer); ok {
				terms.LockBlocks = n
			}
		}
	}
	if err := tokenizer.Err(); err != nil {
		return nil, fmt.Errorf("parse bond script: %w", err)
	}

	if !haveCSV {
		return nil, fmt.Errorf("script has no timelock; a bond without one is not locked up at all")
	}
	if err := checkPubKey(terms.Owner); err != nil {
		return nil, fmt.Errorf("bond owner key: %w", err)
	}
	if terms.LockBlocks == 0 {
		return nil, fmt.Errorf("bond script has no lock length")
	}

	// Rebuilding is the real check: it rejects anything with extra opcodes,
	// a second signature check, or any other path to the coin, which reading
	// opcode by opcode would let through.
	want, err := BondScript(terms.Owner, terms.LockBlocks)
	if err != nil {
		return nil, err
	}
	if len(want) != len(bond) {
		return nil, fmt.Errorf("script is not a bond: it has %d bytes, a bond on these terms has %d",
			len(bond), len(want))
	}
	for i := range want {
		if want[i] != bond[i] {
			return nil, fmt.Errorf("script is not a bond on the terms it appears to carry")
		}
	}
	return &terms, nil
}

// BondAddress returns the P2SH address a bond is posted to, and the pkScript
// that funds it.
func BondAddress(bond []byte, params stdaddr.AddressParams) (stdaddr.Address, []byte, error) {
	return Address(bond, params)
}

// smallIntOrData reads a pushed number, whether it arrived as a small-integer
// opcode or a data push.
func smallIntOrData(tok txscript.ScriptTokenizer) (uint32, bool) {
	op := tok.Opcode()
	if op == txscript.OP_0 {
		return 0, true
	}
	if op >= txscript.OP_1 && op <= txscript.OP_16 {
		return uint32(op-txscript.OP_1) + 1, true
	}
	data := tok.Data()
	if len(data) == 0 || len(data) > 4 {
		return 0, false
	}
	var n uint32
	for i := len(data) - 1; i >= 0; i-- {
		n = n<<8 | uint32(data[i])
	}
	return n, true
}
