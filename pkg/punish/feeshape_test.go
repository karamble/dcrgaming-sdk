package punish

import (
	"bytes"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdscript"
	"github.com/decred/dcrd/wire"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
)

// A faithful local mirror of the bridge's broadcast shape rules, copied from
// dcrpulse dashboard/internal/services/gaming_broadcast.go (signaturesIn,
// outputsMayPayAnyone, passesThrough, maxPassThroughFee) - copied rather than
// imported, because the bridge stays a tunnel and this repo must not depend on
// it. The bridge admits three shapes and no others: co-signed (every input
// carries two or more signatures, outputs unchecked), passing-through (every
// output a script hash the wallet does not hold, fee within the cap), and
// coming home (every output the wallet's own). These tests prove which shape
// each punishment-path spend needs, so a bridge policy drift shows up here as
// a failing mirror rather than a stranded spend on a live table.

const (
	mirrorSigLen            = 65
	mirrorMaxPassThroughFee = 100_000
	mirrorScriptVersion     = 0
)

// mirrorSignaturesIn counts how many parties had to agree to a spend: the
// pushes before the trailing redeem script that are the size of a signature.
func mirrorSignaturesIn(sigScript []byte) int {
	var pushes [][]byte
	tok := txscript.MakeScriptTokenizer(mirrorScriptVersion, sigScript)
	for tok.Next() {
		if d := tok.Data(); d != nil {
			pushes = append(pushes, d)
		}
	}
	if tok.Err() != nil || len(pushes) < 2 {
		return 0
	}
	var n int
	for _, p := range pushes[:len(pushes)-1] {
		if len(p) == mirrorSigLen {
			n++
		}
	}
	return n
}

// mirrorCoSigned is the bridge's outputsMayPayAnyone: every input needed more
// than one signature, and not otherwise.
func mirrorCoSigned(tx *wire.MsgTx) bool {
	if len(tx.TxIn) == 0 {
		return false
	}
	for _, in := range tx.TxIn {
		if mirrorSignaturesIn(in.SignatureScript) < 2 {
			return false
		}
	}
	return true
}

// mirrorPassesThrough is the bridge's passesThrough: coin that was never the
// wallet's, staying not the wallet's, with nothing over the fee cap taken on
// the way. The bridge reads script class and ownership from its wallet's
// decoder; the mirror reads the same facts from the scripts directly.
func mirrorPassesThrough(tx *wire.MsgTx, inputAtoms int64, walletOwns func([]byte) bool) bool {
	if len(tx.TxIn) == 0 || len(tx.TxOut) == 0 || inputAtoms <= 0 {
		return false
	}
	var outputAtoms int64
	for _, out := range tx.TxOut {
		outputAtoms += out.Value
	}
	for _, out := range tx.TxOut {
		if !stdscript.IsScriptHashScript(mirrorScriptVersion, out.PkScript) || walletOwns(out.PkScript) {
			return false
		}
	}
	fee := inputAtoms - outputAtoms
	return fee >= 0 && fee <= mirrorMaxPassThroughFee
}

// mirrorShape is the bridge's decision order: co-signed first, then
// passing-through, and everything else must come home.
func mirrorShape(tx *wire.MsgTx, inputAtoms int64, walletOwns func([]byte) bool) string {
	if mirrorCoSigned(tx) {
		return "co-signed"
	}
	if mirrorPassesThrough(tx, inputAtoms, walletOwns) {
		return "pass-through"
	}
	return "home"
}

// The fee-shape proof for the whole punishment path: the answer leg at the
// gv1 fee of 10_000 clears passing-through; the accuse leg is co-signed and
// is never judged as passing-through; the take and the forfeit sweep clear
// neither, so their single output to the pinned payout - the bridge wallet's
// own - is the only shape a bridge will relay for them.
func TestPunishmentSpendsFitTheBridgeShapes(t *testing.T) {
	params := chaincfg.TestNet3Params()
	pinned := pinnedScript()
	// The bridge wallet holds exactly the pinned payout.
	walletOwns := func(pk []byte) bool { return bytes.Equal(pk, pinned) }

	// The ladder's first rung, answered and taken.
	lf := newLadderFixture(t)
	d := lf.accuseDraft()
	chain, err := BuildLadder(d)
	if err != nil {
		t.Fatalf("build ladder: %v", err)
	}
	sigs := make([][]byte, 0, 2)
	for _, m := range lf.members {
		sig, err := escrow.SignBondSpend(chain[0], lf.bond, lf.keyFor(t, m))
		if err != nil {
			t.Fatalf("co-sign: %v", err)
		}
		sigs = append(sigs, sig)
	}
	accuse, err := CoSignAccuse(chain[0], lf.bond, sigs, params)
	if err != nil {
		t.Fatalf("finish accuse: %v", err)
	}
	answerDraft := escrow.AnswerDraft{
		Claimed:    lf.claimed,
		Bond:       lf.bond,
		Prevout:    wire.OutPoint{Hash: accuse.TxHash(), Index: 0, Tree: wire.TxTreeRegular},
		ValueAtoms: 990_000,
		FeeAtoms:   10_000,
		Params:     params,
	}
	answer, err := AnswerClaim(lf.keyA, answerDraft)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	take, err := TakeExpired(lf.keyB, pinned, Take{
		Claimed:    lf.claimed,
		Prevout:    wire.OutPoint{Hash: accuse.TxHash(), Index: 0, Tree: wire.TxTreeRegular},
		ValueAtoms: 990_000,
		FeeAtoms:   10_000,
		Params:     params,
	})
	if err != nil {
		t.Fatalf("take: %v", err)
	}

	// The forfeit sweep from an equivocation.
	sf := newSweepFixture(t)
	digA, sigA := storyHalf(t, sf.logPriv, "shape tale one")
	digB, sigB := storyHalf(t, sf.logPriv, "shape tale two")
	recovered, err := forfeit.Recover(sf.logPriv.PubKey(), digA, sigA, digB, sigB)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	sweep, err := SweepForfeited(recovered, pinned, sweepDraft(sf))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// The answer leg: single-sig, into a script hash that is not the
	// wallet's, leaving exactly the gv1 fee of 10_000 - passing-through.
	if fee := int64(990_000) - answer.TxOut[0].Value; fee != 10_000 {
		t.Fatalf("the answer leg leaves a fee of %d, want 10000", fee)
	}
	if got := mirrorShape(answer, 990_000, walletOwns); got != "pass-through" {
		t.Fatalf("the answer leg is %q to the bridge, want pass-through", got)
	}

	// The accuse leg: two signatures on its one input - co-signed, and never
	// evaluated as passing-through, because the bridge decides co-signed first.
	if n := mirrorSignaturesIn(accuse.TxIn[0].SignatureScript); n != 2 {
		t.Fatalf("the accuse leg carries %d signatures, want 2", n)
	}
	if got := mirrorShape(accuse, 1_000_000, walletOwns); got != "co-signed" {
		t.Fatalf("the accuse leg is %q to the bridge, want co-signed", got)
	}

	// The take and the sweep: one signature, so never co-signed, and their
	// output is the wallet's own pinned payout, so never passing-through.
	// Coming home is the only door left, and it opens only because every
	// output is the pinned script.
	for _, tc := range []struct {
		name       string
		tx         *wire.MsgTx
		inputAtoms int64
	}{
		{"take", take, 990_000},
		{"forfeit sweep", sweep, 1_000_000},
	} {
		if got := mirrorShape(tc.tx, tc.inputAtoms, walletOwns); got != "home" {
			t.Fatalf("the %s is %q to the bridge, want home", tc.name, got)
		}
		for i, out := range tc.tx.TxOut {
			if !walletOwns(out.PkScript) {
				t.Fatalf("the %s output %d pays a script the bridge wallet does not hold; it would be refused", tc.name, i)
			}
		}
	}

	// The cap is real: the same answer shape one atom over it no longer
	// passes through.
	over := answerDraft
	over.FeeAtoms = 100_001
	overAnswer, err := AnswerClaim(lf.keyA, over)
	if err != nil {
		t.Fatalf("over-fee answer: %v", err)
	}
	if mirrorPassesThrough(overAnswer, 990_000, walletOwns) {
		t.Fatal("an answer leg over the fee cap still passed through")
	}
}
