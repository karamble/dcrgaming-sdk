package escrow

import "testing"

// A timelock beyond the sequence mask is not a script that matures late - it is
// one that never matures, because consensus compares only the low 16 bits of
// the spending input's sequence. Coin paid into it is unrecoverable by anybody,
// so both builders have to refuse rather than hand back a script that looks
// perfectly well formed.
func TestATimelockNothingCouldEverSpendIsRefused(t *testing.T) {
	_, pubs := memberKeys(t, 1)
	owner := pubs[0]

	if _, err := RedeemScript(owner, [][]byte{owner}, MaxCSVBlocks+1); err == nil {
		t.Fatal("built an escrow whose refund branch no sequence could satisfy")
	}
	if _, err := BondScript(owner, MaxCSVBlocks+1); err == nil {
		t.Fatal("built a bond no sequence could ever reclaim")
	}

	// The boundary itself is spendable and must stay allowed.
	if _, err := RedeemScript(owner, [][]byte{owner}, MaxCSVBlocks); err != nil {
		t.Fatalf("refused the largest lock that can actually be spent: %v", err)
	}
	if _, err := BondScript(owner, MaxCSVBlocks); err != nil {
		t.Fatalf("refused the largest bond lock that can actually be spent: %v", err)
	}
}
