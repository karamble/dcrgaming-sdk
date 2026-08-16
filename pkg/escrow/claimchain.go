package escrow

import (
	"bytes"
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
)

// The three transactions a dispute is made of, and the chain of them agreed up
// front.
//
// A claim moves the bond into a claimed bond and takes nothing. The owner answers
// by spending that back into a bond of its own, alone. If it does not, the others
// take it once the window closes. Only the first needs agreeing in advance, and it
// needs everybody - see ClaimedBondScript for why that is the trade.

// AccuseDraft is the transaction that opens a dispute.
//
// It pays one output, to the claimed bond derived from the bond it spends, and
// that is the whole of it: no amounts to divide, nowhere to name. Every peer
// builds the same bytes from the bond alone, which is what makes CheckAccuseDraft
// worth running - it is the only thing standing where a covenant would be, since
// nothing on chain can require this output to be what it should.
type AccuseDraft struct {
	// Bond is the table bond script being spent.
	Bond []byte
	// Prevout and ValueAtoms identify it. The value is not bookkeeping:
	// Decred's signature hash commits to it, so a wrong one produces
	// signatures that verify against nothing.
	Prevout    wire.OutPoint
	ValueAtoms int64
	FeeAtoms   int64
	Params     stdaddr.AddressParams
}

// ClaimedScript is the claimed bond an accusation on this bond pays into.
//
// Derived from the bond's own terms rather than supplied, so no caller can name
// anywhere else and every peer reaches the same script.
func (d AccuseDraft) ClaimedScript() ([]byte, error) {
	terms, err := ParseTableBond(d.Bond)
	if err != nil {
		return nil, err
	}
	return ClaimedBondScript(terms.Owner, terms.Members, terms.ClaimBlocks)
}

// BuildAccuse builds the unsigned accusation.
//
// No timelock on the input: under the bond this spends, the branch that moves it
// needs every member and waits for nothing. The waiting is downstream now.
func BuildAccuse(d AccuseDraft) (*wire.MsgTx, error) {
	claimed, err := d.ClaimedScript()
	if err != nil {
		return nil, err
	}
	_, pkScript, err := Address(claimed, d.Params)
	if err != nil {
		return nil, fmt.Errorf("derive the claimed bond's address: %w", err)
	}
	if d.ValueAtoms <= 0 {
		return nil, fmt.Errorf("the bond being claimed holds nothing")
	}
	if d.FeeAtoms < 0 {
		return nil, fmt.Errorf("a negative fee")
	}
	if d.FeeAtoms >= d.ValueAtoms {
		return nil, fmt.Errorf("a fee of %d leaves nothing of a %d bond", d.FeeAtoms, d.ValueAtoms)
	}

	tx := wire.NewMsgTx()
	tx.Version = 3
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: d.Prevout,
		ValueIn:          d.ValueAtoms,
	})
	tx.AddTxOut(wire.NewTxOut(d.ValueAtoms-d.FeeAtoms, pkScript))
	return tx, nil
}

// CheckAccuseDraft is what a member runs before pre-signing an accusation.
//
// Signed long before anybody looks at it again, and against their own bond as
// often as anybody else's, so the check is not a formality: an accusation paying
// anywhere but into the claimed bond is one its target could never answer, and a
// signature given now cannot be taken back.
func CheckAccuseDraft(tx *wire.MsgTx, d AccuseDraft) error {
	want, err := BuildAccuse(d)
	if err != nil {
		return err
	}
	if tx == nil || len(tx.TxIn) != 1 || len(tx.TxOut) != 1 {
		return fmt.Errorf("an accusation has one input and one output")
	}
	if tx.Version != want.Version {
		return fmt.Errorf("accusation is version %d, want %d", tx.Version, want.Version)
	}
	if tx.TxIn[0].PreviousOutPoint != want.TxIn[0].PreviousOutPoint {
		return fmt.Errorf("accusation spends a different output than the bond named")
	}
	if tx.TxIn[0].ValueIn != want.TxIn[0].ValueIn {
		return fmt.Errorf("accusation states the bond holds %d, not %d",
			tx.TxIn[0].ValueIn, want.TxIn[0].ValueIn)
	}
	if tx.TxIn[0].Sequence != want.TxIn[0].Sequence {
		return fmt.Errorf("accusation carries a sequence of %d, and the branch it spends has no timelock",
			tx.TxIn[0].Sequence)
	}
	if tx.TxOut[0].Value != want.TxOut[0].Value {
		return fmt.Errorf("accusation pays out %d, and the fee leaves %d",
			tx.TxOut[0].Value, want.TxOut[0].Value)
	}
	if !bytes.Equal(tx.TxOut[0].PkScript, want.TxOut[0].PkScript) {
		return fmt.Errorf("accusation pays somewhere other than into the claimed bond, " +
			"so the seat it accuses could never answer it")
	}
	return nil
}

// AnswerDraft is the accused seat's reply: the claimed bond, back into a bond.
//
// Back into a bond rather than to a payout address, so that answering keeps the
// seat bonded. Otherwise an accusation would be a way to get your bond out
// mid-table, and abandoning afterwards would cost nothing.
type AnswerDraft struct {
	// Claimed is the claimed bond script being spent, and Bond is the table
	// bond to pay back into.
	Claimed    []byte
	Bond       []byte
	Prevout    wire.OutPoint
	ValueAtoms int64
	FeeAtoms   int64
	Params     stdaddr.AddressParams
}

// BuildAnswer builds the answer, which its owner signs alone.
//
// Deterministic like every other draft here, and for a reason particular to this
// one: the next accusation in the chain spends what this produces, so its bytes
// have to be known before anybody has answered anything.
func BuildAnswer(d AnswerDraft) (*wire.MsgTx, error) {
	claimed, err := ParseClaimedBond(d.Claimed)
	if err != nil {
		return nil, err
	}
	bond, err := ParseTableBond(d.Bond)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(claimed.Owner, bond.Owner) {
		return nil, fmt.Errorf("the claimed bond and the bond it answers into have different owners")
	}
	_, pkScript, err := Address(d.Bond, d.Params)
	if err != nil {
		return nil, fmt.Errorf("derive the bond's address: %w", err)
	}
	if d.ValueAtoms <= 0 {
		return nil, fmt.Errorf("the claimed bond holds nothing")
	}
	if d.FeeAtoms < 0 {
		return nil, fmt.Errorf("a negative fee")
	}
	if d.FeeAtoms >= d.ValueAtoms {
		return nil, fmt.Errorf("a fee of %d leaves nothing of a %d bond", d.FeeAtoms, d.ValueAtoms)
	}

	tx := wire.NewMsgTx()
	tx.Version = 3
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: d.Prevout,
		ValueIn:          d.ValueAtoms,
		// The answer branch has no timelock, so nothing to satisfy.
	})
	tx.AddTxOut(wire.NewTxOut(d.ValueAtoms-d.FeeAtoms, pkScript))
	return tx, nil
}

// TakeDraft is the accusers' spend of a claimed bond whose window has closed.
//
// Assembled at the time rather than agreed in advance: the seats taking it are
// all willing then, which is what being the accusers means.
type TakeDraft struct {
	Claimed    []byte
	Prevout    wire.OutPoint
	ValueAtoms int64
	// PayScripts is where each taking seat is paid, in the canonical order the
	// claimed bond lists them. The order decides the bytes and therefore what
	// is being signed, so it comes from the script rather than from whoever
	// assembled the message.
	PayScripts [][]byte
	FeeAtoms   int64
}

// BuildTake builds the forfeiture.
func BuildTake(d TakeDraft) (*wire.MsgTx, error) {
	terms, err := ParseClaimedBond(d.Claimed)
	if err != nil {
		return nil, err
	}
	if len(d.PayScripts) != len(terms.Others) {
		return nil, fmt.Errorf("this claimed bond pays %d seats, and the draft names %d",
			len(terms.Others), len(d.PayScripts))
	}
	for i, p := range d.PayScripts {
		if len(p) == 0 {
			return nil, fmt.Errorf("seat %d has nowhere to be paid", i)
		}
	}
	if d.ValueAtoms <= 0 {
		return nil, fmt.Errorf("the claimed bond holds nothing")
	}
	if d.FeeAtoms < 0 {
		return nil, fmt.Errorf("a negative fee")
	}
	shares, err := Shares(d.ValueAtoms-d.FeeAtoms, len(terms.Others))
	if err != nil {
		return nil, err
	}

	tx := wire.NewMsgTx()
	tx.Version = 3
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: d.Prevout,
		ValueIn:          d.ValueAtoms,
		// The forfeiture branch is timelocked and this is what satisfies
		// it, so a take built without it fails at the engine rather than at
		// broadcast.
		Sequence: terms.ClaimBlocks,
	})
	for i, p := range d.PayScripts {
		tx.AddTxOut(wire.NewTxOut(shares[i], p))
	}
	return tx, nil
}

// AffordableDepth is how many accusations a bond of this size can actually fund.
//
// Each step of the chain costs two fees, not one: the accusation moves the bond and
// the answer moves it back. What has to be left at the end is enough for the
// forfeiture to pay every taking seat its minimum, or the last accusation in the
// chain is one nobody could ever collect on.
//
// Worth computing rather than assuming. A chain that does not fit is a chain that
// cannot be built at all, and the failure is silent from the outside: a table would
// simply have no accusations agreed and nobody would be told.
func AffordableDepth(value, fee int64, takers int) int {
	if value <= 0 || fee <= 0 || takers < 1 {
		return 0
	}
	room := value - int64(takers)*MinShareAtoms
	if room <= 0 {
		return 0
	}
	depth := int(room / (2 * fee))
	if depth > AccuseDepth {
		return AccuseDepth
	}
	return depth
}

// BuildAccuseChain builds every accusation a table may need against one bond, in
// order.
//
// The first spends the bond where it is now. Each one after it spends the bond
// that answering the one before produces, which is knowable in advance because
// Decred hashes a transaction's prefix and its witness separately - the identity
// of the answer is fixed before its owner signs it, so every outpoint in the chain
// is known before anything is signed.
//
// A seat accuses with the entry whose input is where the bond actually sits, so a
// peer that has forgotten how many have been used finds its place by looking at
// the chain rather than by counting.
func BuildAccuseChain(d AccuseDraft, depth int) ([]*wire.MsgTx, error) {
	if depth < 1 {
		return nil, fmt.Errorf("a chain of %d accusations is no chain", depth)
	}
	if depth > AccuseDepth {
		return nil, fmt.Errorf("a chain of %d accusations is beyond the %d agreed", depth, AccuseDepth)
	}
	claimed, err := d.ClaimedScript()
	if err != nil {
		return nil, err
	}

	out := make([]*wire.MsgTx, 0, depth)
	next := d
	for i := range depth {
		accuse, err := BuildAccuse(next)
		if err != nil {
			return nil, fmt.Errorf("accusation %d: %w", i, err)
		}
		out = append(out, accuse)

		// Where the bond sits after this accusation is answered: the answer
		// spends the accusation's output and pays back into the same bond.
		answer, err := BuildAnswer(AnswerDraft{
			Claimed:    claimed,
			Bond:       next.Bond,
			Prevout:    wire.OutPoint{Hash: accuse.TxHash(), Index: 0, Tree: wire.TxTreeRegular},
			ValueAtoms: accuse.TxOut[0].Value,
			FeeAtoms:   next.FeeAtoms,
			Params:     next.Params,
		})
		if err != nil {
			return nil, fmt.Errorf("the answer after accusation %d: %w", i, err)
		}
		next.Prevout = wire.OutPoint{Hash: answer.TxHash(), Index: 0, Tree: wire.TxTreeRegular}
		next.ValueAtoms = answer.TxOut[0].Value
	}
	return out, nil
}

// SignClaimedSpend signs a spend of a claimed bond, for either of its branches.
//
// Separate from SignBondSpend only because the script it commits to is a
// different one: the signature hash covers the redeem script, so signing against
// the bond would produce something that verifies against nothing.
func SignClaimedSpend(tx *wire.MsgTx, claimed []byte, key *secp256k1.PrivateKey) ([]byte, error) {
	if tx == nil {
		return nil, fmt.Errorf("no transaction to sign")
	}
	if key == nil {
		return nil, fmt.Errorf("no signing key")
	}
	if _, err := ParseClaimedBond(claimed); err != nil {
		return nil, err
	}
	sighash, err := txscript.CalcSignatureHash(claimed, txscript.SigHashAll, tx, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("signature hash: %w", err)
	}
	sig, err := schnorr.Sign(key, sighash)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	return append(sig.Serialize(), byte(txscript.SigHashAll)), nil
}
