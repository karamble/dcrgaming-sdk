package punish

import (
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
)

// The clean end of a table (spec 8.4). Each seat's bond goes home through the
// co-signed alive branch, and an owner whose counterpart withholds that
// signature waits out the bond's own lock and leaves through the backstop
// alone. Either way the bond pays its owner: the release is the one spend in
// this package with no pinned sweep in it, because nothing is being taken.

// Release describes sending one seat's table bond home. OwnerPay is where the
// bond's owner asked to be paid; there is no pinned payout here because
// nothing is forfeited.
type Release struct {
	// Bond is the seat's table bond redeem script.
	Bond       []byte
	Prevout    wire.OutPoint
	ValueAtoms int64

	// OwnerPay is the owner's own payout script.
	OwnerPay []byte
	FeeAtoms int64

	Params stdaddr.AddressParams
}

// BuildRelease builds the unsigned cooperative release for both seats to sign.
func BuildRelease(r Release) (*wire.MsgTx, error) {
	if _, err := twoSeats(r.Bond); err != nil {
		return nil, err
	}
	return escrow.BuildAlive(escrow.AliveDraft{
		Bond:       r.Bond,
		Prevout:    r.Prevout,
		ValueAtoms: r.ValueAtoms,
		PayScript:  r.OwnerPay,
		FeeAtoms:   r.FeeAtoms,
	})
}

// CoSignRelease finishes the release with both seats' signatures in canonical
// member order. It is the same alive branch an accusation spends: which of
// the two happened is decided by which pre-agreed transaction the signatures
// complete, not by the script.
func CoSignRelease(tx *wire.MsgTx, bond []byte, sigs [][]byte, params stdaddr.AddressParams) (*wire.MsgTx, error) {
	return CoSignAccuse(tx, bond, sigs, params)
}

// BackstopRelease is the owner's way out when the co-sign is withheld: one
// signature through the backstop branch, spendable only once the bond's own
// lock matures. The lock goes into the input's sequence and the result is
// checked by the real script engine before it is handed back.
func BackstopRelease(key *secp256k1.PrivateKey, r Release) (*wire.MsgTx, error) {
	terms, err := twoSeats(r.Bond)
	if err != nil {
		return nil, err
	}
	return escrow.BuildTimelockedSpend(escrow.Spend{
		Key:        key,
		Script:     r.Bond,
		Prevout:    r.Prevout,
		ValueAtoms: r.ValueAtoms,
		CSVBlocks:  terms.LockBlocks,
		PayScript:  r.OwnerPay,
		FeeAtoms:   r.FeeAtoms,
		SigScript:  escrow.BackstopSigScript,
		Params:     r.Params,
	})
}
