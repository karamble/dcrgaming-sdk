package bridgetest

import (
	"decred.org/dcrwallet/v5/wallet/txrules"
	"github.com/decred/dcrd/dcrutil/v4"

	"github.com/karamble/dcrgaming-sdk/pkg/finance"
)

// RelayFees are dcrwallet's default relay fee and dust rules, the same ones
// a dcrpulse bridge builds payouts with. A game test that settles a table
// passes them in Options.Fees.
func RelayFees() finance.FeeRules {
	return finance.FeeRules{
		Fee: func(size int) int64 {
			return int64(txrules.FeeForSerializeSize(txrules.DefaultRelayFeePerKb, size))
		},
		Dust: func(atoms int64, script []byte) bool {
			return txrules.IsDustAmount(dcrutil.Amount(atoms), len(script), txrules.DefaultRelayFeePerKb)
		},
	}
}
