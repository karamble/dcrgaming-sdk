package forfeit

import (
	"bytes"
	"crypto/hmac"
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"
)

// The fixtures below are independent literals, never values the code under
// test produced for itself, so a derivation change cannot regenerate them to
// match - it can only fail here, before it strands a bond a restarted client
// can no longer punish through.

const (
	// goldenPunishSeed is the 32-byte seed the pins derive from.
	goldenPunishSeed = "8f2a55908785c7fa46e0d5a1c0414bf95a3a90bd617f8ac86293d1e04ac2b108"

	// goldenOpponentA and goldenOpponentB are two valid compressed session
	// keys: the curve generator and its double, chosen because they are
	// literals nobody derived here.
	goldenOpponentA = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	goldenOpponentB = "02c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5"

	// goldenPunishPub pins PunishmentKeyFrom(seed, match, opponentA).
	goldenPunishPub = "03a68241a56397c0bd39ec1cbf6973eeda1a26fc39ec2616613fc0b9f9ba835bf0"
)

func pubFromHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 33 {
		t.Fatalf("the literal %q is not a 33-byte compressed key", s)
	}
	return b
}

func seedFromHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("the literal %q is not a 32-byte seed", s)
	}
	return b
}

// The preimage is assembled byte by byte from literals and run through the
// same HMAC by hand, so the layout and the value are both pinned: one byte
// moved and a restarted client derives a key that is in no bond on chain.
func TestThePunishmentKeyPreimageLayoutIsPinned(t *testing.T) {
	seed := seedFromHex(t, goldenPunishSeed)
	opp := pubFromHex(t, goldenOpponentA)

	pre := make([]byte, 0, 133)
	pre = append(pre, "gaming/forfeit/punish/v1"...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x40) // be32(64), the match id's length
	pre = append(pre, match...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x21) // be32(33), the opponent key's length
	pre = append(pre, opp...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x00) // be32(0), the first counter
	if len(pre) != 133 {
		t.Fatalf("the pinned preimage is %d bytes, want 133, so this test's own assembly is wrong", len(pre))
	}

	mac := hmac.New(blake256.New, seed)
	mac.Write(pre)
	var raw [32]byte
	copy(raw[:], mac.Sum(nil))
	var d secp256k1.ModNScalar
	if overflow := d.SetBytes(&raw); overflow != 0 || d.IsZero() {
		t.Fatal("the fixture lands on the counter walk's skip case; pick another seed")
	}
	byHand := secp256k1.NewPrivateKey(&d)
	if got := hex.EncodeToString(byHand.PubKey().SerializeCompressed()); got != goldenPunishPub {
		t.Fatalf("the assembled preimage derives %s, want the pinned %s", got, goldenPunishPub)
	}

	priv, err := PunishmentKeyFrom(seed, match, opp)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if got := hex.EncodeToString(priv.PubKey().SerializeCompressed()); got != goldenPunishPub {
		t.Fatalf("PunishmentKeyFrom derives %s, want the pinned %s", got, goldenPunishPub)
	}
}

// A restart must re-derive the same key, and no two (match, opponent) pairs
// may share one: a shared punishment key is a shared branch half.
func TestPunishmentKeysAreDeterministicAndDistinct(t *testing.T) {
	seed := seedFromHex(t, goldenPunishSeed)
	oppA := pubFromHex(t, goldenOpponentA)
	oppB := pubFromHex(t, goldenOpponentB)

	first, err := PunishmentKeyFrom(seed, match, oppA)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	again, err := PunishmentKeyFrom(seed, match, oppA)
	if err != nil {
		t.Fatalf("derive again: %v", err)
	}
	if !bytes.Equal(first.Serialize(), again.Serialize()) {
		t.Fatal("one match and one opponent derived two different keys")
	}

	seen := map[string]string{}
	for _, tc := range []struct {
		name string
		m    string
		opp  []byte
	}{
		{"match A opponent A", match, oppA},
		{"match A opponent B", match, oppB},
		{"match B opponent A", otherMatch, oppA},
		{"match B opponent B", otherMatch, oppB},
	} {
		priv, err := PunishmentKeyFrom(seed, tc.m, tc.opp)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		id := string(priv.PubKey().SerializeCompressed())
		if was, ok := seen[id]; ok {
			t.Fatalf("%s derived the same key as %s", tc.name, was)
		}
		seen[id] = tc.name
	}

	otherSeed := seedFromHex(t, "1e99423a4ed27608a15a2616a2b0e9e52ced330ac530edcc32c8ffc6a526aedd")
	other, err := PunishmentKeyFrom(otherSeed, match, oppA)
	if err != nil {
		t.Fatalf("derive from the other seed: %v", err)
	}
	if bytes.Equal(other.Serialize(), first.Serialize()) {
		t.Fatal("two seeds derived one key")
	}
}

func TestPunishmentKeyFromRefusesBadInputs(t *testing.T) {
	seed := seedFromHex(t, goldenPunishSeed)
	opp := pubFromHex(t, goldenOpponentA)

	for _, tc := range []struct {
		name string
		seed []byte
		m    string
		opp  []byte
	}{
		{"no seed", nil, match, opp},
		{"a short seed", seed[:31], match, opp},
		{"a long seed", append(append([]byte(nil), seed...), 0x00), match, opp},
		{"no match", seed, "", opp},
		{"a match of only space", seed, "   ", opp},
		{"a match with surrounding space", seed, " " + match, opp},
		{"no opponent", seed, match, nil},
		{"a truncated opponent key", seed, match, opp[:32]},
		{"an opponent key off the curve", seed, match, append([]byte{0x02}, bytes.Repeat([]byte{0xff}, 32)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PunishmentKeyFrom(tc.seed, tc.m, tc.opp); err == nil {
				t.Fatal("derived a key from input that should have been refused")
			}
		})
	}
}

// The adaptive-P fixture, all literals: the attacker reads the announced log
// key and names P = p'G - L, which collapses a plain sum to a key it holds
// alone. The branch key that actually comes out is pinned, and pinned again
// through the coefficient path, so the weighting cannot quietly become the
// plain sum while every relative assertion still passes.
func TestTheAdaptivePunishmentKeyFixtureIsPinned(t *testing.T) {
	const goldenAdaptiveForfeit = "03fc438c4b7a8a29f4e53ba6ba3b2d8cb152d484e359c5c59fed120f1a230406d0"

	l := privFromHex(t, "7d9f0e6a3c81b5240fa6d2e89b174c53a02e8f6d1b49c7a35e60d8f214a7b909")
	pPrime := privFromHex(t, "4a1c8d2e97f0b6533d18e5a2c46f90b7215d3e8fa60c49d7128b5f0e63a4d180")
	L := l.PubKey()
	br := Branch{Match: match, Seat: pubFromHex(t, goldenOpponentB)}

	P := addPub(t, pPrime.PubKey(), negPub(L))
	if !addPub(t, L, P).IsEqual(pPrime.PubKey()) {
		t.Fatal("this test did not build the attack it is named for")
	}

	F, err := ForfeitKey(br, L, P)
	if err != nil {
		t.Fatalf("forfeit key: %v", err)
	}
	if got := hex.EncodeToString(F.SerializeCompressed()); got != goldenAdaptiveForfeit {
		t.Fatalf("the fixture derives %s, want the pinned %s", got, goldenAdaptiveForfeit)
	}

	// The same key again through the coefficient path, so the pin above is a
	// pin on coefficient's layout and not just on ForfeitKey's arithmetic.
	cl, cp, err := coefficients(br, L, P)
	if err != nil {
		t.Fatalf("coefficients: %v", err)
	}
	if cl.Equals(cp) {
		t.Fatal("both halves took one weight, so the adaptive announcement cancels the log key")
	}
	if !addPub(t, mulPub(t, cl, L), mulPub(t, cp, P)).IsEqual(F) {
		t.Fatal("the branch key is not the weighted sum its coefficients describe")
	}

	// Everything the attacker can assemble from the one scalar it knows.
	m := digest("take the bond")
	for _, cand := range []struct {
		name string
		d    *secp256k1.ModNScalar
	}{
		{"p' itself", new(secp256k1.ModNScalar).Set(&pPrime.Key)},
		{"c_P * p'", new(secp256k1.ModNScalar).Mul2(cp, &pPrime.Key)},
		{"c_L * p'", new(secp256k1.ModNScalar).Mul2(cl, &pPrime.Key)},
	} {
		priv := secp256k1.NewPrivateKey(cand.d)
		if priv.PubKey().IsEqual(F) {
			t.Fatalf("%s is the branch key, so the attacker spends a bond nobody forfeited", cand.name)
		}
		sig, err := schnorr.Sign(priv, m)
		if err != nil {
			t.Fatalf("sign with %s: %v", cand.name, err)
		}
		if sig.Verify(m, F) {
			t.Fatalf("%s signs for the branch key", cand.name)
		}
	}
}

// The two degenerate announcements, pinned to literals: the log key itself
// hands the owner both halves, and its negation weights to a key the owner
// signs for alone. Both must be refused, and an ordinary neighbouring point
// must not be, or the refusal is a broken builder wearing a safety's name.
func TestTheDegenerateAnnouncementFixturesAreRefused(t *testing.T) {
	l := privFromHex(t, "7d9f0e6a3c81b5240fa6d2e89b174c53a02e8f6d1b49c7a35e60d8f214a7b909")
	L := l.PubKey()
	br := Branch{Match: match, Seat: pubFromHex(t, goldenOpponentB)}

	if _, err := ForfeitKey(br, L, L); err == nil {
		t.Fatal("a punishment key equal to the log key got a branch")
	}
	if _, err := ForfeitKey(br, L, negPub(L)); err == nil {
		t.Fatal("a punishment key that is the negated log key got a branch")
	}

	honest, err := secp256k1.ParsePubKey(pubFromHex(t, goldenOpponentA))
	if err != nil {
		t.Fatalf("parse the honest fixture: %v", err)
	}
	if _, err := ForfeitKey(br, L, honest); err != nil {
		t.Fatalf("an ordinary punishment key was refused: %v", err)
	}
}
