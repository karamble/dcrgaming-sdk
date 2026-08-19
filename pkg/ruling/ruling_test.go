package ruling

import (
	"reflect"
	"testing"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
)

func key(t *testing.T) *secp256k1.PrivateKey {
	t.Helper()
	k, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return k
}

func digest(s string) []byte {
	h := blake256.Sum256([]byte(s))
	return h[:]
}

func at(seq uint64) forfeit.Position {
	return forfeit.Position{Match: "m", Domain: forfeit.DomainEntry, Seq: seq}
}

// equivocation signs two different statements at one position, which is the
// cheat the whole punishment path exists for.
func equivocation(t *testing.T, k *secp256k1.PrivateKey, seq uint64) *Equivocated {
	t.Helper()
	a, b := digest("seat 1 says left"), digest("seat 1 says right")
	sigA, err := forfeit.Sign(k, at(seq), a)
	if err != nil {
		t.Fatalf("sign a: %v", err)
	}
	sigB, err := forfeit.Sign(k, at(seq), b)
	if err != nil {
		t.Fatalf("sign b: %v", err)
	}
	return &Equivocated{
		At:    at(seq),
		Pub:   k.PubKey().SerializeCompressed(),
		HashA: a, SigA: sigA,
		HashB: b, SigB: sigB,
	}
}

// The guarantee: the game's assertion is not what moves the bond.
func TestAnEquivocationRulingRecoversTheSignersKey(t *testing.T) {
	k := key(t)
	e := equivocation(t, k, 4)

	r := Ruling{Match: "m", Against: 1, Kind: Equivocation, Equivocated: e}
	if err := r.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	got, err := e.Recover()
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !got.PubKey().IsEqual(k.PubKey()) {
		t.Fatal("recovered a key that is not the one that signed twice")
	}
}

// Two statements signed at DIFFERENT positions are two honest signatures. They
// do not share a nonce, so no key falls out and no bond may be swept.
func TestTwoHonestSignaturesExposeNothing(t *testing.T) {
	k := key(t)
	a, b := digest("seat 1 moves"), digest("seat 1 moves again")
	sigA, err := forfeit.Sign(k, at(4), a)
	if err != nil {
		t.Fatalf("sign a: %v", err)
	}
	sigB, err := forfeit.Sign(k, at(5), b)
	if err != nil {
		t.Fatalf("sign b: %v", err)
	}
	e := &Equivocated{
		At: at(4), Pub: k.PubKey().SerializeCompressed(),
		HashA: a, SigA: sigA, HashB: b, SigB: sigB,
	}
	if _, err := e.Recover(); err == nil {
		t.Fatal("recovered a key from two honest signatures")
	}
}

// A ruling naming someone else's key must not recover, even if the signatures
// themselves are a real equivocation by a third party. The check itself lives
// in forfeit.Recover; this pins the behaviour at the boundary a game sees, so a
// refactor that stops passing the named key through is caught here.
func TestARulingCannotBorrowAnotherSeatsEquivocation(t *testing.T) {
	cheat, other := key(t), key(t)
	e := equivocation(t, cheat, 7)
	e.Pub = other.PubKey().SerializeCompressed()

	if _, err := e.Recover(); err == nil {
		t.Fatal("recovered a key for a seat that did not sign")
	}
}

func TestOnlyTheProofTheKindNamesMaySit(t *testing.T) {
	k := key(t)
	e := equivocation(t, k, 1)
	s := &Silent{Duty: "place", Seq: 3, By: 900}

	for _, tc := range []struct {
		name string
		r    Ruling
		ok   bool
	}{
		{"clean carries nothing", Ruling{Match: "m", Kind: Clean}, true},
		{"clean with a proof", Ruling{Match: "m", Kind: Clean, Silent: s}, false},
		{"equivocation with its proof", Ruling{Match: "m", Kind: Equivocation, Equivocated: e}, true},
		{"equivocation without one", Ruling{Match: "m", Kind: Equivocation}, false},
		{"equivocation with the wrong one", Ruling{Match: "m", Kind: Equivocation, Silent: s}, false},
		{"silence with its proof", Ruling{Match: "m", Kind: Silence, Silent: s}, true},
		{"silence without one", Ruling{Match: "m", Kind: Silence}, false},
		{"both proofs at once", Ruling{Match: "m", Kind: Equivocation, Equivocated: e, Silent: s}, false},
		{"no match", Ruling{Kind: Clean}, false},
		{"no such kind", Ruling{Match: "m", Kind: Kind(9)}, false},
	} {
		err := tc.r.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: refused a good ruling: %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: accepted a bad ruling", tc.name)
		}
	}
}

func TestTheSameStatementTwiceIsNotEquivocation(t *testing.T) {
	k := key(t)
	same := digest("seat 1 moves")
	sig, err := forfeit.Sign(k, at(2), same)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	e := &Equivocated{
		At: at(2), Pub: k.PubKey().SerializeCompressed(),
		HashA: same, SigA: sig, HashB: same, SigB: sig,
	}
	if err := e.validate(); err == nil {
		t.Fatal("accepted one statement signed once as a cheat")
	}
}

func TestASilenceRulingMustSayWhatAndWhen(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Silent
	}{
		{"no duty", Silent{Seq: 1, By: 900}},
		{"no height", Silent{Duty: "place", Seq: 1}},
	} {
		r := Ruling{Match: "m", Kind: Silence, Silent: &tc.s}
		if err := r.Validate(); err == nil {
			t.Errorf("%s: accepted an unanswerable silence ruling", tc.name)
		}
	}
}

// A ruling names a seat, a kind and a proof. It must never gain a field that
// names money, because then a wrong ruling could direct a payment rather than
// merely punish the wrong seat of its own table. This pins the shape.
func TestARulingNamesNoMoney(t *testing.T) {
	for _, tc := range []struct {
		what any
		want []string
	}{
		{Ruling{}, []string{"Match", "Against", "Kind", "Equivocated", "Silent"}},
		{Equivocated{}, []string{"At", "Pub", "HashA", "SigA", "HashB", "SigB"}},
		{Silent{}, []string{"Duty", "Seq", "By"}},
	} {
		typ := reflect.TypeOf(tc.what)
		var got []string
		for i := 0; i < typ.NumField(); i++ {
			got = append(got, typ.Field(i).Name)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s fields are %v, want %v - if a field naming a script, "+
				"address, amount or transaction was added, it does not belong here",
				typ.Name(), got, tc.want)
		}
	}
}

// The spike's decision rule: one ruling type covers both games' punishment
// paths with no game-specific branch in the SDK. Attrition is absent on
// purpose - it is what running Silence produces when the accused keeps
// answering, an outcome rather than an input.
func TestBothGamesFitTheThreeKinds(t *testing.T) {
	for _, tc := range []struct {
		game, path string
		want       Kind
	}{
		{"battleships", "divergent cell attestations sweep the bond", Equivocation},
		{"battleships", "a seat stops answering its duty clock", Silence},
		{"battleships", "the match ends cleanly, table bond released", Clean},
		{"poker", "a seat signs two actions at one position", Equivocation},
		{"poker", "an unanswered claim is taken after its window", Silence},
		{"poker", "the table breaks up, bonds released", Clean},
	} {
		if tc.want > Silence {
			t.Fatalf("%s/%s: mapped to a kind that does not exist", tc.game, tc.path)
		}
		r := Ruling{Match: "m", Against: 1, Kind: tc.want}
		switch tc.want {
		case Equivocation:
			r.Equivocated = equivocation(t, key(t), 1)
		case Silence:
			r.Silent = &Silent{Duty: "owed", Seq: 1, By: 900}
		}
		if err := r.Validate(); err != nil {
			t.Errorf("%s/%s: %v", tc.game, tc.path, err)
		}
	}
}
