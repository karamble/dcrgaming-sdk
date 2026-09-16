package escrow

import (
	"bytes"
	"fmt"
	"github.com/karamble/dcrgaming-sdk/pkg/finance"

	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
)

// Consensus bounds only; the invitation supplies the actual amount and delay.
const MinBondBlocks uint32 = 1
const MinBondAtoms uint64 = 1
const BondConfirmations uint32 = 2
const StakeConfirmations uint32 = 1

// BondTerms is what a bond script commits to.
type BondTerms struct {
	Owner []byte
	// Recovery is separate from the game identity for bridge-controlled bonds.
	Recovery   []byte
	LockBlocks uint32
}

// ParseBond reads a bond script back, so a referee can check that a deposit a
// player points at really is a bond of theirs on the terms claimed rather than
// some other script that happens to hold coin.
func ParseBond(bond []byte) (*BondTerms, error) {
	tok := txscript.MakeScriptTokenizer(0, bond)
	if !tok.Next() || tok.Opcode() != txscript.OP_DATA_33 {
		return nil, fmt.Errorf("bridge-owned bond required")
	}
	identity := append([]byte(nil), tok.Data()...)
	if !tok.Next() || tok.Opcode() != txscript.OP_DROP || !tok.Next() {
		return nil, fmt.Errorf("invalid admission bond")
	}
	lock, ok := smallIntOrData(tok)
	if !ok || lock <= 0 || lock > finance.MaxLockBlocks {
		return nil, fmt.Errorf("invalid refund lock")
	}
	if !tok.Next() || tok.Opcode() != txscript.OP_CHECKSEQUENCEVERIFY || !tok.Next() || tok.Opcode() != txscript.OP_DROP || !tok.Next() || tok.Opcode() != txscript.OP_DATA_33 {
		return nil, fmt.Errorf("invalid refund branch")
	}
	recovery := append([]byte(nil), tok.Data()...)
	expected, err := finance.AdmissionScript(identity, recovery, uint32(lock))
	if err != nil || !bytes.Equal(expected, bond) {
		return nil, fmt.Errorf("noncanonical admission bond")
	}
	return &BondTerms{Owner: identity, Recovery: recovery, LockBlocks: uint32(lock)}, nil
}

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

// BridgeBondScript binds the game's admission identity to a bond whose only
// spending key is controlled independently by the bridge. The dropped identity
// is committed by the P2SH hash and used exclusively for proof of possession.
func BridgeBondScript(identity, recovery []byte, blocks uint32) ([]byte, error) {
	return finance.AdmissionScript(identity, recovery, blocks)
}
