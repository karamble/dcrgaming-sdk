package escrow

import (
	"bytes"
	"fmt"

	"github.com/decred/dcrd/txscript/v4"
)

// Where a claimed bond waits while its owner has the chance to answer.
//
// The window has to start when the accusation does. A timelock on the bond
// itself cannot do that: OP_CHECKSEQUENCEVERIFY counts from the output being
// spent, and the bond is confirmed before the table will deal - so by the time
// anybody is accused of leaving, the lock is long satisfied and an accusation
// carries no delay at all.
//
// So the claim does not take the bond. It moves it here, and the delay is on
// this output, which the accusation itself creates.
//
//	OP_IF   <owner> 2 OP_CHECKSIGALTVERIFY OP_TRUE            # the answer
//	OP_ELSE <claimBlocks> CSV DROP <every other member> ...   # the forfeiture
//
// Two things follow, and the second is worth more than the first.
//
// The answer is immediate and the forfeiture is not, so an accused player who is
// still there always wins - not by being faster, but because the other branch
// cannot be taken yet.
//
// And the answer needs the owner's key and nothing else. Under a bond whose alive
// branch answers a claim, an answer needs every member's signature including the
// accusers', who will not give it once they have started - so it has to be agreed
// in advance, can be agreed exactly once, and has to be kept somewhere. A seat
// that lost that could not answer at all. Here a seat answers from its own key, so
// the ability to answer survives losing everything but the seed.
//
// What moves in exchange is which side needs agreeing in advance: the claim does,
// by everybody, because nothing on chain can require a claim's output to be this
// script. Decred has no covenants, so the only claim that can exist is one the
// whole table signed while it was still cooperating. The owner signs it willingly:
// they are the one holding the interrupt.
func ClaimedBondScript(owner []byte, members [][]byte, claimBlocks uint32) ([]byte, error) {
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
		return nil, fmt.Errorf("a claimed bond needs at least 2 members, not %d", len(sorted))
	}
	if claimBlocks == 0 {
		return nil, fmt.Errorf("a claim with no delay could not be answered")
	}
	if claimBlocks > MaxCSVBlocks {
		return nil, fmt.Errorf("a claim delay of %d blocks is beyond %d", claimBlocks, MaxCSVBlocks)
	}
	others := withoutKey(sorted, owner)

	b := txscript.NewScriptBuilder()
	b.AddOp(txscript.OP_IF).
		AddData(owner).
		AddInt64(sigType).
		AddOp(txscript.OP_CHECKSIGALTVERIFY).
		AddOp(txscript.OP_ELSE).
		AddInt64(int64(claimBlocks)).
		AddOp(txscript.OP_CHECKSEQUENCEVERIFY).
		AddOp(txscript.OP_DROP)
	for _, m := range others {
		b.AddData(m).AddInt64(sigType).AddOp(txscript.OP_CHECKSIGALTVERIFY)
	}
	b.AddOp(txscript.OP_ENDIF).AddOp(txscript.OP_TRUE)

	script, err := b.Script()
	if err != nil {
		return nil, err
	}
	if len(script) > txscript.MaxScriptElementSize {
		return nil, fmt.Errorf("claimed bond script is %d bytes, over the %d a P2SH push allows",
			len(script), txscript.MaxScriptElementSize)
	}
	return script, nil
}

// ClaimedBondTerms is what a claimed bond says, read back from the script.
type ClaimedBondTerms struct {
	Owner       []byte
	Others      [][]byte // canonical order, owner excluded
	ClaimBlocks uint32
}

// ParseClaimedBond reads a claimed bond back, and rebuilds it to check.
//
// The accused runs this on the output a claim produced before believing it can be
// answered. Rebuilding is what makes it worth running: a script that merely looks
// like this one, with a second way out or somebody else's key in the answer
// branch, holds real coin and would leave the owner unable to answer at all.
func ParseClaimedBond(script []byte) (*ClaimedBondTerms, error) {
	tokenizer := txscript.MakeScriptTokenizer(scriptVersion, script)

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
		return nil, fmt.Errorf("parse claimed bond script: %w", err)
	}
	if len(locks) != 1 {
		return nil, fmt.Errorf("a claimed bond has one timelock; this has %d", len(locks))
	}
	if len(keys) < 2 {
		return nil, fmt.Errorf("script names %d keys, which is not a claimed bond's layout", len(keys))
	}

	// The owner comes first, being the answer branch, and everybody else
	// follows in the forfeiture.
	owner := keys[0]
	others := keys[1:]

	// Rebuilding is the real check: it is the only thing that rules out a
	// script carrying these same pushes beside another path to the coin.
	want, err := ClaimedBondScript(owner, append([][]byte{owner}, others...), locks[0])
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(want, script) {
		return nil, fmt.Errorf("script is not a claimed bond on the terms it appears to carry")
	}
	return &ClaimedBondTerms{Owner: owner, Others: others, ClaimBlocks: locks[0]}, nil
}

// AnswerSigScript spends a claimed bond back to its owner, immediately.
//
// One signature, the owner's own. That is the whole point of this script: a seat
// answers an accusation without asking anybody, so the ability to answer cannot be
// taken away by withholding a signature or lost by forgetting a file.
func AnswerSigScript(script, ownerSig []byte) ([]byte, error) {
	if _, err := ParseClaimedBond(script); err != nil {
		return nil, err
	}
	return branchSigScript(script, [][]byte{ownerSig}, txscript.OP_1)
}

// TakeSigScript takes a claimed bond once the window has closed.
//
// Every member except the owner signs, and the spending input's sequence has to
// satisfy ClaimBlocks. Unlike the answer, this needs no agreement in advance: the
// seats taking it are all willing at the time, which is what being the accusers
// means.
func TakeSigScript(script []byte, sigs [][]byte) ([]byte, error) {
	terms, err := ParseClaimedBond(script)
	if err != nil {
		return nil, err
	}
	if len(sigs) != len(terms.Others) {
		return nil, fmt.Errorf("a forfeiture needs all %d other members' signatures, not %d",
			len(terms.Others), len(sigs))
	}
	return branchSigScript(script, sigs, txscript.OP_0)
}
