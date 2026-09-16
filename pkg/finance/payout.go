package finance

import (
	"fmt"
	"sort"

	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
)

type Payment struct {
	Key   string `json:"key"`
	Atoms int64  `json:"atoms"`
}

// Payout is a game proposal, never authorization. Payments are the gross game
// outcome and must account for every deposited atom. The bridge obtains payout
// destinations from its authenticated roster and applies the network fee.
type Payout struct {
	Table    string    `json:"table"`
	Inputs   []Input   `json:"inputs"`
	Payments []Payment `json:"payments"`
}

// DefaultRelayFeeAtomsPerKB is the financial protocol's deterministic relay
// fee. Every bridge independently derives the same fee from the worst-case
// signed transaction size; games cannot select or inflate it.
const DefaultRelayFeeAtomsPerKB int64 = 10_000

// BuiltPayout is the exact bridge-derived transaction and accounting presented
// to the operator. Payments are the net on-chain outputs after the fee.
type BuiltPayout struct {
	Transaction *wire.MsgTx
	Inputs      []Input
	Payments    []Payment
	FeeAtoms    int64
	Size        int
}

// BuildPayout canonicalizes ordering, applies the protocol fee and constructs
// the only transaction the operator may approve. The caller still verifies the
// descriptor and current chain facts independently.
func BuildPayout(p Payout, destinations map[string]string, params stdaddr.AddressParams) (BuiltPayout, error) {
	var empty BuiltPayout
	if p.Table == "" || len(p.Inputs) < 2 || len(p.Inputs) > MaxMembers || len(p.Payments) == 0 || len(p.Payments) > MaxMembers {
		return empty, fmt.Errorf("invalid payout shape")
	}
	inputs := append([]Input(nil), p.Inputs...)
	// Normalize before ordering: hex casing must not change the transaction
	// participants independently reconstruct and approve.
	for i := range inputs {
		terms, err := inputs[i].Terms.Canonical()
		if err != nil {
			return empty, err
		}
		inputs[i].Terms = terms
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Terms.Recovery < inputs[j].Terms.Recovery })
	tx := wire.NewMsgTx()
	tx.Version = 3
	var total int64
	seen := map[wire.OutPoint]bool{}
	owners := map[string]bool{}
	var roster []string
	for i, input := range inputs {
		terms := input.Terms
		if terms.Table != p.Table || terms.Kind != "stake" || seen[input.Outpoint] || owners[terms.Recovery] || input.Outpoint.Tree != wire.TxTreeRegular || terms.Atoms > MaxAtoms-total {
			return empty, fmt.Errorf("invalid payout input")
		}
		if i == 0 {
			roster = terms.Members
		}
		if len(roster) != len(terms.Members) {
			return empty, fmt.Errorf("inconsistent payout roster")
		}
		for n, key := range roster {
			if terms.Members[n] != key {
				return empty, fmt.Errorf("inconsistent payout roster")
			}
		}
		owners[terms.Recovery] = true
		seen[input.Outpoint] = true
		total += terms.Atoms
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: input.Outpoint, ValueIn: terms.Atoms, Sequence: wire.MaxTxInSequenceNum, BlockHeight: wire.NullBlockHeight, BlockIndex: wire.NullBlockIndex})
	}
	if len(inputs) != len(roster) || len(destinations) != len(roster) {
		return empty, fmt.Errorf("payout must cover the complete funded roster")
	}
	for _, key := range roster {
		if !owners[key] || destinations[key] == "" {
			return empty, fmt.Errorf("payout lacks a funded member or authenticated destination")
		}
	}
	payments := append([]Payment(nil), p.Payments...)
	sort.Slice(payments, func(i, j int) bool { return payments[i].Key < payments[j].Key })
	paid := int64(0)
	last := ""
	for _, payment := range payments {
		if !owners[payment.Key] || payment.Key == last || payment.Atoms <= 0 || payment.Atoms > MaxAtoms-paid {
			return empty, fmt.Errorf("invalid payout recipient or amount")
		}
		last = payment.Key
		paid += payment.Atoms
		address, err := stdaddr.DecodeAddress(destinations[payment.Key], params)
		if err != nil {
			return empty, err
		}
		version, pk := address.PaymentScript()
		if version != 0 {
			return empty, fmt.Errorf("unsupported payout script version")
		}
		tx.AddTxOut(wire.NewTxOut(payment.Atoms, pk))
	}
	if total != paid {
		return empty, fmt.Errorf("gross payout does not conserve funds")
	}

	// Estimate the complete transaction with the largest canonical DER
	// signatures. Actual low-S signatures can only make it smaller.
	for i, input := range inputs {
		script, err := input.Terms.Script()
		if err != nil {
			return empty, err
		}
		b := txscript.NewScriptBuilder()
		for range input.Terms.Members {
			b.AddData(make([]byte, 73)) // 72-byte DER signature plus hash type.
		}
		witness, err := b.AddOp(txscript.OP_TRUE).AddData(script).Script()
		if err != nil {
			return empty, err
		}
		tx.TxIn[i].SignatureScript = witness
	}
	size := tx.SerializeSize()
	fee := DefaultRelayFeeAtomsPerKB * int64(size) / 1000
	if fee <= 0 || fee > total {
		return empty, fmt.Errorf("invalid bridge-derived payout fee")
	}
	for _, input := range tx.TxIn {
		input.SignatureScript = nil
	}

	share, remainder := fee/int64(len(payments)), fee%int64(len(payments))
	for i := range payments {
		deduction := share
		if i == 0 {
			deduction += remainder
		}
		payments[i].Atoms -= deduction
		if payments[i].Atoms <= 0 || dustPayment(payments[i].Atoms, tx.TxOut[i].PkScript) {
			return empty, fmt.Errorf("payout output cannot cover bridge-derived fee")
		}
		tx.TxOut[i].Value = payments[i].Atoms
	}
	return BuiltPayout{Transaction: tx, Inputs: inputs, Payments: payments, FeeAtoms: fee, Size: size}, nil
}

func dustPayment(atoms int64, script []byte) bool {
	// Matches the standard default-relay dust calculation for a compressed
	// P2PKH redeem input. It deliberately uses the protocol relay fee above.
	size := 8 + 2 + wire.VarIntSerializeSize(uint64(len(script))) + len(script) + 165
	return atoms*1000/(3*int64(size)) < DefaultRelayFeeAtomsPerKB
}
