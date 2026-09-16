package finance

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
)

func payoutFixture(t *testing.T) (Payout, map[string]string) {
	t.Helper()
	first, keys := fixture(t, "stake")
	second := first
	second.Outpoint.Index++
	second.Terms.Recovery = hex.EncodeToString(keys[2].PubKey().SerializeCompressed())
	destinations := map[string]string{}
	for _, key := range keys[1:] {
		pub := key.PubKey().SerializeCompressed()
		addr, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(stdaddr.Hash160(pub), chaincfg.SimNetParams())
		if err != nil {
			t.Fatal(err)
		}
		destinations[hex.EncodeToString(pub)] = addr.String()
	}
	return Payout{Table: first.Terms.Table, Inputs: []Input{first, second}, Payments: []Payment{{Key: first.Terms.Recovery, Atoms: 2000000}}}, destinations
}

func TestPayoutCanonicalOrdering(t *testing.T) {
	p, destinations := payoutFixture(t)
	expected, err := BuildPayout(p, destinations, chaincfg.SimNetParams())
	if err != nil {
		t.Fatal(err)
	}
	original := p.Inputs[1].Terms.Recovery
	// These fixture keys have the same prefix and a letter at their first
	// differing digit. Uppercase sorting previously reversed their order.
	p.Inputs[1].Terms.Recovery = strings.ToUpper(original)
	p.Inputs[0], p.Inputs[1] = p.Inputs[1], p.Inputs[0]
	actual, err := BuildPayout(p, destinations, chaincfg.SimNetParams())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := actual.Transaction.Bytes()
	b, _ := expected.Transaction.Bytes()
	if !bytes.Equal(a, b) {
		t.Fatal("equivalent key encodings changed payout bytes")
	}
	if p.Inputs[0].Terms.Recovery != strings.ToUpper(original) {
		t.Fatal("caller input mutated")
	}
	for _, input := range actual.Inputs {
		if input.Terms.Recovery != strings.ToLower(input.Terms.Recovery) {
			t.Fatal("noncanonical returned input")
		}
	}
}

func TestPayoutRejectsInvalidProposals(t *testing.T) {
	cases := map[string]func(*Payout, map[string]string){
		"duplicate input":     func(p *Payout, _ map[string]string) { p.Inputs[1].Outpoint = p.Inputs[0].Outpoint },
		"duplicate owner":     func(p *Payout, _ map[string]string) { p.Inputs[1].Terms.Recovery = p.Inputs[0].Terms.Recovery },
		"foreign table":       func(p *Payout, _ map[string]string) { p.Inputs[1].Terms.Table = "other" },
		"missing member":      func(p *Payout, _ map[string]string) { p.Inputs = p.Inputs[:1] },
		"wrong amount":        func(p *Payout, _ map[string]string) { p.Payments[0].Atoms++ },
		"overflow":            func(p *Payout, _ map[string]string) { p.Payments[0].Atoms = 1<<63 - 1 },
		"foreign recipient":   func(p *Payout, _ map[string]string) { p.Payments[0].Key = p.Inputs[0].Terms.Identity },
		"duplicate recipient": func(p *Payout, _ map[string]string) { p.Payments = append(p.Payments, p.Payments[0]) },
		"missing destination": func(p *Payout, d map[string]string) { delete(d, p.Inputs[1].Terms.Recovery) },
		"wrong network destination": func(p *Payout, d map[string]string) {
			pub, _ := hex.DecodeString(p.Payments[0].Key)
			a, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(stdaddr.Hash160(pub), chaincfg.MainNetParams())
			if err != nil {
				t.Fatal(err)
			}
			d[p.Payments[0].Key] = a.String()
		},
	}
	for name, alter := range cases {
		t.Run(name, func(t *testing.T) {
			p, destinations := payoutFixture(t)
			alter(&p, destinations)
			if _, err := BuildPayout(p, destinations, chaincfg.SimNetParams()); err == nil {
				t.Fatal("invalid proposal accepted")
			}
		})
	}
}

func TestPayoutFeeIsBridgeDerived(t *testing.T) {
	p, destinations := payoutFixture(t)
	built, err := BuildPayout(p, destinations, chaincfg.SimNetParams())
	if err != nil {
		t.Fatal(err)
	}
	if built.FeeAtoms != DefaultRelayFeeAtomsPerKB*int64(built.Size)/1000 || built.FeeAtoms <= 0 {
		t.Fatalf("fee %d does not match worst-case size %d", built.FeeAtoms, built.Size)
	}
	if len(built.Payments) != 1 || built.Payments[0].Atoms != 2_000_000-built.FeeAtoms || built.Transaction.TxOut[0].Value != built.Payments[0].Atoms {
		t.Fatal("derived fee was not applied to exact payout output")
	}
	for _, in := range built.Transaction.TxIn {
		if len(in.SignatureScript) != 0 {
			t.Fatal("fee estimator leaked dummy signatures into unsigned payout")
		}
	}
}

func TestPayoutFeeCoversEverySupportedRosterSize(t *testing.T) {
	for count := 2; count <= MaxMembers; count++ {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			keys := make([]*secp256k1.PrivateKey, count)
			members := make([]string, count)
			destinations := make(map[string]string, count)
			for i := range keys {
				raw := make([]byte, 32)
				raw[30], raw[31] = byte(i>>8), byte(i+1)
				keys[i] = secp256k1.PrivKeyFromBytes(raw)
				members[i] = hex.EncodeToString(keys[i].PubKey().SerializeCompressed())
			}
			sort.Strings(members)
			byPublic := make(map[string]*secp256k1.PrivateKey, count)
			for _, key := range keys {
				public := hex.EncodeToString(key.PubKey().SerializeCompressed())
				byPublic[public] = key
				address, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(stdaddr.Hash160(key.PubKey().SerializeCompressed()), chaincfg.SimNetParams())
				if err != nil {
					t.Fatal(err)
				}
				destinations[public] = address.String()
			}
			proposal := Payout{Table: "fee-table"}
			for i, owner := range members {
				identityRaw := make([]byte, 32)
				identityRaw[0], identityRaw[31] = 1, byte(i+1)
				identity := secp256k1.PrivKeyFromBytes(identityRaw)
				var hash chainhash.Hash
				hash[0], hash[1] = byte(count), byte(i+1)
				terms := Terms{Version: Version, Game: "test", Network: "simnet", Table: proposal.Table, Kind: "stake", Atoms: 100_000_000, LockBlocks: 288, Identity: hex.EncodeToString(identity.PubKey().SerializeCompressed()), Recovery: owner, Members: members}
				proposal.Inputs = append(proposal.Inputs, Input{Terms: terms, Outpoint: wire.OutPoint{Hash: hash}})
			}
			proposal.Payments = []Payment{{Key: members[0], Atoms: int64(count) * 100_000_000}}
			built, err := BuildPayout(proposal, destinations, chaincfg.SimNetParams())
			if err != nil {
				t.Fatal(err)
			}
			if built.FeeAtoms != DefaultRelayFeeAtomsPerKB*int64(built.Size)/1000 {
				t.Fatalf("fee %d does not cover estimated size %d", built.FeeAtoms, built.Size)
			}
			for i, input := range built.Inputs {
				signatures := make(map[string][]byte, count)
				hash, err := SignatureHash(built.Transaction, i, input)
				if err != nil {
					t.Fatal(err)
				}
				for _, member := range members {
					signatures[member] = ecdsa.Sign(byPublic[member], hash).Serialize()
				}
				built.Transaction.TxIn[i].SignatureScript, err = SettlementWitness(built.Transaction, i, input, signatures)
				if err != nil {
					t.Fatal(err)
				}
			}
			if actual := built.Transaction.SerializeSize(); actual > built.Size {
				t.Fatalf("signed size %d exceeds fee estimate %d", actual, built.Size)
			}
			if err = VerifySpend(built.Transaction, built.Inputs, chaincfg.SimNetParams()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
