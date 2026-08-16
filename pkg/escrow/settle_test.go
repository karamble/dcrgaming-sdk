package escrow

import (
	"bytes"
	"testing"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/wire"
)

// The transaction that moves coin between players, as opposed to the several
// that move somebody's own coin back to them. It is the only reason any of the
// rest of this exists, so most of what follows is about chips not appearing or
// vanishing on the way.

type settling struct {
	privs   []*secp256k1.PrivateKey
	members [][]byte
	draft   SettleDraft
}

// settleTable stakes n players and prepares a payout of the given amounts.
func settleTable(t *testing.T, stake int64, amounts []int64) *settling {
	t.Helper()
	n := len(amounts)
	privs, pubs := memberKeys(t, n)
	members, err := CanonicalMembers(pubs)
	if err != nil {
		t.Fatalf("members: %v", err)
	}

	s := &settling{privs: privs, members: members}
	for i := range n {
		// Each seat's own escrow: the settlement branch names the table,
		// the refund branch names only this seat.
		redeem, err := RedeemScript(members[i], members, testCSVBlocks)
		if err != nil {
			t.Fatalf("seat %d redeem: %v", i, err)
		}
		var h chainhash.Hash
		copy(h[:], bytes.Repeat([]byte{byte(i + 1)}, chainhash.HashSize))
		s.draft.Inputs = append(s.draft.Inputs, SettleInput{
			Redeem:     redeem,
			Prevout:    wire.OutPoint{Hash: h, Index: 0, Tree: wire.TxTreeRegular},
			ValueAtoms: stake,
		})
		s.draft.Pays = append(s.draft.Pays, payTo(t, n)[i])
	}
	s.draft.Amounts = append([]int64(nil), amounts...)
	s.draft.FeeAtoms = testBondFee
	return s
}

// sign has every member sign every input, which is what the settlement branch of
// each seat's script requires.
func (s *settling) sign(t *testing.T, tx *wire.MsgTx) [][][]byte {
	t.Helper()
	perMember := make([][][]byte, 0, len(s.members))
	for _, m := range s.members {
		sigs, err := SignSettlement(tx, s.draft, privFor(t, s.privs, m))
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		perMember = append(perMember, sigs)
	}
	// Transposed: the sig script for an input wants every member's
	// signature for that input, in canonical member order.
	byInput := make([][][]byte, len(s.draft.Inputs))
	for i := range byInput {
		for m := range perMember {
			byInput[i] = append(byInput[i], perMember[m][i])
		}
	}
	return byInput
}

// A table pays out what it ended holding, and the result satisfies every seat's
// escrow.
func TestATablePaysOutWhatItEndedHolding(t *testing.T) {
	const stake = int64(1_000_000)
	for _, amounts := range [][]int64{
		{1_500_000, 500_000},                   // heads-up, one ahead
		{2_000_000, 0},                         // one player has it all
		{1_200_000, 900_000, 900_000},          // three seats
		{2_500_000, 700_000, 500_000, 300_000}, // four
	} {
		s := settleTable(t, stake, amounts)
		tx, err := BuildSettlement(s.draft)
		if err != nil {
			t.Fatalf("%v: build: %v", amounts, err)
		}
		done, err := FinishSettlement(tx, s.draft, s.sign(t, tx), chaincfg.TestNet3Params())
		if err != nil {
			t.Fatalf("%v: finish: %v", amounts, err)
		}

		// Every atom is accounted for: what the seats staked comes out
		// again, less the fee and nothing else.
		var in, out int64
		for _, i := range done.TxIn {
			in += i.ValueIn
		}
		for _, o := range done.TxOut {
			out += o.Value
		}
		if in-out != testBondFee {
			t.Fatalf("%v: %d went in and %d came out, and the fee is %d",
				amounts, in, out, testBondFee)
		}
		// A seat holding nothing gets no output rather than one of zero.
		busted := 0
		for _, a := range amounts {
			if a == 0 {
				busted++
			}
		}
		if len(done.TxOut) != len(amounts)-busted {
			t.Fatalf("%v: paid %d seats, want %d", amounts, len(done.TxOut), len(amounts)-busted)
		}
	}
}

// Chips may not appear or vanish. A settlement that pays out more than the table
// holds is refused by the network; one that pays out less quietly hands the
// difference to a miner.
func TestASettlementCannotCreateOrDestroyChips(t *testing.T) {
	const stake = int64(1_000_000)
	for _, tc := range []struct {
		name    string
		amounts []int64
	}{
		{"paying out more than was staked", []int64{1_500_000, 900_000}},
		{"paying out less than was staked", []int64{1_000_000, 500_000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := settleTable(t, stake, tc.amounts)
			if _, err := BuildSettlement(s.draft); err == nil {
				t.Fatal("a settlement was built that does not balance")
			}
		})
	}
}

// Every seat's stake needs every seat's signature. A settlement short of one is
// short of the whole thing, which is what stops any subset of a table paying
// itself out.
func TestASettlementNeedsEverySeat(t *testing.T) {
	s := settleTable(t, 1_000_000, []int64{1_200_000, 800_000})
	tx, err := BuildSettlement(s.draft)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	good := s.sign(t, tx)
	params := chaincfg.TestNet3Params()

	t.Run("one seat signing twice for another", func(t *testing.T) {
		bad := [][][]byte{{good[0][0], good[0][0]}, good[1]}
		if _, err := FinishSettlement(tx, s.draft, bad, params); err == nil {
			t.Fatal("a settlement stood up with one seat signing twice")
		}
	})
	t.Run("signatures in the wrong order", func(t *testing.T) {
		bad := [][][]byte{{good[0][1], good[0][0]}, good[1]}
		if _, err := FinishSettlement(tx, s.draft, bad, params); err == nil {
			t.Fatal("a settlement stood up with its signatures out of order")
		}
	})
	t.Run("a stranger's signature", func(t *testing.T) {
		outsiders, _ := memberKeys(t, 1)
		sigs, err := SignSettlement(tx, s.draft, outsiders[0])
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		bad := [][][]byte{{sigs[0], good[0][1]}, good[1]}
		if _, err := FinishSettlement(tx, s.draft, bad, params); err == nil {
			t.Fatal("a settlement stood up on a stranger's signature")
		}
	})
	t.Run("a signature over another settlement", func(t *testing.T) {
		other := settleTable(t, 1_000_000, []int64{1_100_000, 900_000})
		otherTx, err := BuildSettlement(other.draft)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		sigs, err := SignSettlement(otherTx, other.draft, s.privs[0])
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		bad := [][][]byte{{sigs[0], good[0][1]}, good[1]}
		if _, err := FinishSettlement(tx, s.draft, bad, params); err == nil {
			t.Fatal("a signature over a different settlement was accepted")
		}
	})
}

// A seat asked to sign has to be able to check what it is signing, because
// signing without looking authorises the table's whole balance going elsewhere.
func TestASeatChecksASettlementBeforeSigningIt(t *testing.T) {
	s := settleTable(t, 1_000_000, []int64{1_200_000, 800_000})
	honest, err := BuildSettlement(s.draft)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := CheckSettleDraft(honest, s.draft); err != nil {
		t.Fatalf("an honest settlement did not check out: %v", err)
	}

	for _, tc := range []struct {
		name string
		bend func(*wire.MsgTx)
	}{
		{"paying somewhere else", func(tx *wire.MsgTx) { tx.TxOut[0].PkScript = payTo(t, 9)[8] }},
		{"paying one seat more", func(tx *wire.MsgTx) { tx.TxOut[0].Value += 100_000 }},
		{"quietly taking a fee", func(tx *wire.MsgTx) { tx.TxOut[1].Value -= 50_000 }},
		{"an extra output", func(tx *wire.MsgTx) { tx.AddTxOut(wire.NewTxOut(1000, payTo(t, 9)[8])) }},
		{"a different stake", func(tx *wire.MsgTx) {
			var h chainhash.Hash
			copy(h[:], bytes.Repeat([]byte{0x44}, chainhash.HashSize))
			tx.TxIn[0].PreviousOutPoint.Hash = h
		}},
		{"claiming a stake is bigger than it is", func(tx *wire.MsgTx) {
			tx.TxIn[0].ValueIn *= 2
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad, err := BuildSettlement(s.draft)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			tc.bend(bad)
			if err := CheckSettleDraft(bad, s.draft); err == nil {
				t.Fatal("a seat would have signed a settlement it should have refused")
			}
		})
	}
}

// Every peer must build the same bytes, or the signatures cannot be combined.
func TestEveryPeerBuildsTheSameSettlement(t *testing.T) {
	s := settleTable(t, 1_000_000, []int64{1_800_000, 700_000, 500_000})
	first, err := BuildSettlement(s.draft)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want, err := first.Bytes()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	for i := range 20 {
		again, err := BuildSettlement(s.draft)
		if err != nil {
			t.Fatalf("build %d: %v", i, err)
		}
		got, err := again.Bytes()
		if err != nil {
			t.Fatalf("serialize: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("two builds of one draft produced different settlements")
		}
	}
}

// The host counts signatures to decide what a spend may pay.
//
// dcrpulse relays a game's transactions and will not let one pay anybody but
// its owner unless every input needed more than one signature. It works that
// out generically - the pushes before the redeem script that are the size of a
// signature - because the bridge deliberately knows nothing about poker.
//
// That makes the shape of these two scripts a contract across two repositories
// that no compiler checks. Changing it silently turns every settlement into a
// refusal at the host, on the one path where coin moves without anybody being
// asked, so it is asserted here where the change would be made.
func TestTheHostCanTellWhoHadToSign(t *testing.T) {
	members := make([][]byte, 3)
	for i := range members {
		priv, err := secp256k1.GeneratePrivateKey()
		if err != nil {
			t.Fatalf("key: %v", err)
		}
		members[i] = priv.PubKey().SerializeCompressed()
	}
	redeem, err := RedeemScript(members[0], members, 288)
	if err != nil {
		t.Fatalf("redeem script: %v", err)
	}

	sigs := make([][]byte, len(members))
	for i := range sigs {
		sigs[i] = bytes.Repeat([]byte{byte(i + 1)}, SigLen)
	}
	settle, err := SettlementSigScript(redeem, sigs)
	if err != nil {
		t.Fatalf("settlement sigscript: %v", err)
	}
	refund, err := RefundSigScript(redeem, sigs[0])
	if err != nil {
		t.Fatalf("refund sigscript: %v", err)
	}

	if got := signatureSizedPushes(t, settle); got != len(members) {
		t.Errorf("a settlement shows %d signature-sized pushes, want %d; the host reads "+
			"fewer than two as a spend one party could make alone and will refuse to "+
			"relay its payout", got, len(members))
	}
	if got := signatureSizedPushes(t, refund); got != 1 {
		t.Errorf("a refund shows %d signature-sized pushes, want 1; at two or more the "+
			"host would let a unilateral spend pay somebody other than its owner", got)
	}
}

// signatureSizedPushes counts the way the host does: data pushes the size of a
// signature, excluding the redeem script the script ends with.
func signatureSizedPushes(t *testing.T, sigScript []byte) int {
	t.Helper()
	var pushes [][]byte
	tok := txscript.MakeScriptTokenizer(0, sigScript)
	for tok.Next() {
		if d := tok.Data(); d != nil {
			pushes = append(pushes, d)
		}
	}
	if tok.Err() != nil {
		t.Fatalf("tokenize: %v", tok.Err())
	}
	if len(pushes) < 2 {
		return 0
	}
	var n int
	for _, p := range pushes[:len(pushes)-1] {
		if len(p) == SigLen {
			n++
		}
	}
	return n
}
