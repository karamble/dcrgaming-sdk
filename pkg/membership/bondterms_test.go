package membership

import "testing"

func TestEveryFinancialTermChangesTheDigest(t *testing.T) {
	base := testTerms(2)
	original, err := base.Hash()
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Terms){
		"stake": func(t *Terms) { t.BuyInAtoms++ }, "bond": func(t *Terms) { t.BondAtoms++ }, "bond delay": func(t *Terms) { t.BondLockBlocks++ }, "stake delay": func(t *Terms) { t.CSVBlocks++ }, "deadline": func(t *Terms) { t.Until++ }, "seats": func(t *Terms) { t.Seats++ },
	} {
		t.Run(name, func(t *testing.T) {
			other := base
			change(&other)
			hash, err := other.Hash()
			if err != nil || hash == original {
				t.Fatalf("term not bound: %v", err)
			}
		})
	}
}
func TestBondTermsRequireExplicitAmountAndDelay(t *testing.T) {
	for _, pair := range [][2]uint64{{0, 2016}, {1000000, 0}, {1000000, 65536}, {21000000*100000000 + 1, 2016}} {
		terms := testTerms(2)
		terms.BondAtoms = pair[0]
		terms.BondLockBlocks = uint32(pair[1])
		if err := terms.Validate(); err == nil {
			t.Fatal("invalid bond economics accepted")
		}
	}
}
