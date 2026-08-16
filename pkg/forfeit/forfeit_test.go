package forfeit

import (
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"
)

// The mechanism has exactly two failure modes and they point opposite ways.
//
// If it leaks when it should not, an honest player loses their bond for playing
// correctly, which is worse than having no bond at all. If it fails to leak
// when it should, the whole thing is decoration. So the tests come in pairs:
// every "this leaks" has a "this does not", and the ones that must not leak are
// the ones worth writing first.

const match = "9bbccbcc99e2421852775868835efd6926eab532fb3286f1051f79f7572bb9b9"

// otherMatch is an independent literal, deliberately not derived from match: a
// fixture that derives its bytes from the constant under test tracks the
// constant's mutations, and the test that uses it goes vacuous.
const otherMatch = "3f0c7a1e55d9b84406e2c1fd7ab399215c6e80d4488f13ba0dd5e97c22461af8"

func key(t *testing.T) *secp256k1.PrivateKey {
	t.Helper()
	k, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return k
}

func digest(s string) []byte {
	h := blake256.Sum256([]byte(s))
	return h[:]
}

func at(seq uint64) Position {
	return Position{Match: match, Domain: DomainEntry, Seq: seq}
}

// A signature made this way is an ordinary Decred Schnorr signature. If this
// fails, nothing downstream can verify a log at all.
func TestASignatureVerifiesWithTheStockVerifier(t *testing.T) {
	k := key(t)
	m := digest("seat 1 folds")

	sig, err := Sign(k, at(4), m)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if len(sig) != SigLen {
		t.Fatalf("signature is %d bytes, want %d", len(sig), SigLen)
	}
	parsed, err := schnorr.ParseSignature(sig)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !parsed.Verify(m, k.PubKey()) {
		t.Fatal("a signature made with a positional nonce did not verify")
	}
}

// The whole point: two entries at one position hand over the key.
func TestEquivocationPublishesTheKey(t *testing.T) {
	k := key(t)
	folded := digest("seat 1 folds")
	raised := digest("seat 1 raises 500")

	a, err := Sign(k, at(4), folded)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	b, err := Sign(k, at(4), raised)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	// Both are individually valid, which is what makes the cheat work at
	// all - each recipient sees a perfectly good signature.
	for _, tc := range []struct {
		sig  []byte
		hash []byte
	}{{a, folded}, {b, raised}} {
		p, err := schnorr.ParseSignature(tc.sig)
		if err != nil || !p.Verify(tc.hash, k.PubKey()) {
			t.Fatal("an equivocating signature was not individually valid")
		}
	}

	got, err := Recover(k.PubKey(), folded, a, raised, b)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if !got.Key.Equals(&k.Key) {
		t.Fatal("recovered a key that is not the one that signed")
	}
}

// Playing correctly must never leak. These are the tests that protect the
// honest player, and they matter more than the one above.
func TestPlayingHonestlyDoesNotLeak(t *testing.T) {
	k := key(t)

	t.Run("different positions", func(t *testing.T) {
		a, _ := Sign(k, at(4), digest("folds"))
		b, _ := Sign(k, at(5), digest("raises"))
		if string(a[:32]) == string(b[:32]) {
			t.Fatal("two different sequence numbers produced the same nonce")
		}
		if _, err := Recover(k.PubKey(), digest("folds"), a, digest("raises"), b); err == nil {
			t.Fatal("signatures at different positions exposed the key")
		}
	})

	// The trap this design is most likely to fall into: a seat signs a log
	// entry at seq 5 and a head attestation at seq 5, both legitimately.
	// Without domain separation those share a nonce and the seat publishes
	// its own key by behaving perfectly.
	t.Run("an entry and a head attestation at one sequence", func(t *testing.T) {
		entry := Position{Match: match, Domain: DomainEntry, Seq: 5}
		head := Position{Match: match, Domain: DomainHead, Seq: 5}

		a, _ := Sign(k, entry, digest("seat 1 calls"))
		b, _ := Sign(k, head, digest("head is abcdef"))
		if string(a[:32]) == string(b[:32]) {
			t.Fatal("an entry and a head attestation at one sequence shared a nonce")
		}
		if _, err := Recover(k.PubKey(), digest("seat 1 calls"), a, digest("head is abcdef"), b); err == nil {
			t.Fatal("signing an entry and a head attestation at one sequence exposed the key")
		}
	})

	t.Run("the same message twice", func(t *testing.T) {
		m := digest("seat 1 folds")
		a, _ := Sign(k, at(4), m)
		b, _ := Sign(k, at(4), m)
		if string(a) != string(b) {
			t.Fatal("signing the same thing twice was not deterministic")
		}
		if _, err := Recover(k.PubKey(), m, a, m, b); err == nil {
			t.Fatal("resending an identical signature exposed the key")
		}
	})

	t.Run("the same position in different matches", func(t *testing.T) {
		here := Position{Match: match, Domain: DomainEntry, Seq: 4}
		there := Position{Match: "0000", Domain: DomainEntry, Seq: 4}
		a, _ := Sign(k, here, digest("folds"))
		b, _ := Sign(k, there, digest("raises"))
		if string(a[:32]) == string(b[:32]) {
			t.Fatal("the same position at two matches shared a nonce")
		}
	})

	t.Run("a whole honest hand", func(t *testing.T) {
		sigs := make([][]byte, 0, 40)
		hashes := make([][]byte, 0, 40)
		for seq := uint64(0); seq < 20; seq++ {
			for _, d := range []Domain{DomainEntry, DomainHead} {
				m := digest(string(d) + string(rune('a'+seq)))
				s, err := Sign(k, Position{Match: match, Domain: d, Seq: seq}, m)
				if err != nil {
					t.Fatalf("sign: %v", err)
				}
				sigs = append(sigs, s)
				hashes = append(hashes, m)
			}
		}
		for i := range sigs {
			for j := i + 1; j < len(sigs); j++ {
				if string(sigs[i][:32]) == string(sigs[j][:32]) {
					t.Fatalf("signatures %d and %d reused a nonce during an honest hand", i, j)
				}
				if _, err := Recover(k.PubKey(), hashes[i], sigs[i], hashes[j], sigs[j]); err == nil {
					t.Fatalf("an honest hand exposed the key via signatures %d and %d", i, j)
				}
			}
		}
	})
}

// Different keys at one position must not be confusable either.
func TestAnotherPlayersSignatureDoesNotLeakAnything(t *testing.T) {
	a, b := key(t), key(t)
	ma, mb := digest("a folds"), digest("b raises")

	sa, _ := Sign(a, at(4), ma)
	sb, _ := Sign(b, at(4), mb)

	if string(sa[:32]) == string(sb[:32]) {
		t.Fatal("two different keys produced the same nonce at one position")
	}
	if _, err := Recover(a.PubKey(), ma, sa, mb, sb); err == nil {
		t.Fatal("two players' signatures at one position exposed a key")
	}
}

// Recover must refuse anything it cannot actually solve, rather than returning
// a plausible scalar.
func TestRecoverRefusesWhatItCannotSolve(t *testing.T) {
	k := key(t)
	m := digest("folds")
	sig, _ := Sign(k, at(4), m)

	for _, tc := range []struct {
		name        string
		pub         *secp256k1.PublicKey
		hashA, sigA []byte
		hashB, sigB []byte
	}{
		{"no key", nil, m, sig, m, sig},
		{"a short signature", k.PubKey(), m, sig[:63], m, sig},
		{"a short digest", k.PubKey(), m[:31], sig, m, sig},
		{"the wrong public key", key(t).PubKey(), m, sig, digest("raises"), mustSign(t, k, at(4), digest("raises"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Recover(tc.pub, tc.hashA, tc.sigA, tc.hashB, tc.sigB); err == nil {
				t.Fatal("Recover accepted something it could not have solved")
			}
		})
	}
}

func mustSign(t *testing.T, k *secp256k1.PrivateKey, p Position, m []byte) []byte {
	t.Helper()
	s, err := Sign(k, p, m)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

// A position with no domain or no match is a programming error, not a
// signature - the separation is what keeps honest play safe.
func TestSigningRefusesAnUnqualifiedPosition(t *testing.T) {
	k := key(t)
	for _, p := range []Position{
		{Match: match, Seq: 1},
		{Domain: DomainEntry, Seq: 1},
		{},
	} {
		if _, err := Sign(k, p, digest("x")); err == nil {
			t.Fatalf("signed at an unqualified position %+v", p)
		}
	}
}

// The helpers below do their point arithmetic with this file's own hands, so
// the fixtures they build cannot be vouched for by the code they exist to test.

// privFromHex pins a private key to a literal. A fixture drawn fresh cannot be
// pinned, and one derived from the code under test tracks its mutations.
func privFromHex(t *testing.T, s string) *secp256k1.PrivateKey {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("the literal %q is not a 32-byte scalar", s)
	}
	return secp256k1.PrivKeyFromBytes(b)
}

// negPub is -A.
func negPub(a *secp256k1.PublicKey) *secp256k1.PublicKey {
	var j secp256k1.JacobianPoint
	a.AsJacobian(&j)
	j.Y.Negate(1)
	j.Y.Normalize()
	return secp256k1.NewPublicKey(&j.X, &j.Y)
}

// addPub is A + B, refusing the point at infinity because no fixture here ever
// wants it.
func addPub(t *testing.T, a, b *secp256k1.PublicKey) *secp256k1.PublicKey {
	t.Helper()
	var ja, jb, sum secp256k1.JacobianPoint
	a.AsJacobian(&ja)
	b.AsJacobian(&jb)
	secp256k1.AddNonConst(&ja, &jb, &sum)
	sum.ToAffine()
	if (sum.X.IsZero() && sum.Y.IsZero()) || sum.Z.IsZero() {
		t.Fatal("a fixture summed to the point at infinity")
	}
	return secp256k1.NewPublicKey(&sum.X, &sum.Y)
}

// mulPub is k*A, on a copy of k so no fixture depends on whether the library
// mutates its scalar.
func mulPub(t *testing.T, k *secp256k1.ModNScalar, a *secp256k1.PublicKey) *secp256k1.PublicKey {
	t.Helper()
	kk := new(secp256k1.ModNScalar).Set(k)
	var j, r secp256k1.JacobianPoint
	a.AsJacobian(&j)
	secp256k1.ScalarMultNonConst(kk, &j, &r)
	r.ToAffine()
	if (r.X.IsZero() && r.Y.IsZero()) || r.Z.IsZero() {
		t.Fatal("a fixture multiplied to the point at infinity")
	}
	return secp256k1.NewPublicKey(&r.X, &r.Y)
}

// seatOf is a fresh session key's compressed bytes, which is what a roster
// records for a seat.
func seatOf(t *testing.T) []byte {
	t.Helper()
	return key(t).PubKey().SerializeCompressed()
}

// The forfeit key: neither side can spend alone, both together can.
func TestAForfeitKeyNeedsBothHalves(t *testing.T) {
	log, punisher := key(t), key(t)
	br := Branch{Match: match, Seat: seatOf(t)}

	forfeit, err := ForfeitKey(br, log.PubKey(), punisher.PubKey())
	if err != nil {
		t.Fatalf("forfeit key: %v", err)
	}

	// Neither half alone is the forfeit key, so neither side can sign for it.
	if forfeit.IsEqual(log.PubKey()) || forfeit.IsEqual(punisher.PubKey()) {
		t.Fatal("the forfeit key is one of its halves")
	}

	// Together they are.
	both, err := ForfeitPrivKey(br, log, punisher)
	if err != nil {
		t.Fatalf("forfeit priv: %v", err)
	}
	if !both.PubKey().IsEqual(forfeit) {
		t.Fatal("the two halves did not reconstruct the forfeit key")
	}

	// And it really does sign for it.
	m := digest("take the bond")
	sig, err := schnorr.Sign(both, m)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !sig.Verify(m, forfeit) {
		t.Fatal("the reconstructed key does not sign for the forfeit key")
	}
}

// The role byte is the entire difference between the two weights. If they ever
// come out equal, F collapses to c*(L+P) and a chosen punishment key cancels
// the log key with one byte missing. Nothing else asks this question so
// directly, so this test is not redundant with the attack tests below.
func TestTheTwoHalvesTakeDifferentCoefficients(t *testing.T) {
	log, punisher := key(t), key(t)
	br := Branch{Match: match, Seat: seatOf(t)}

	cl, cp, err := coefficients(br, log.PubKey(), punisher.PubKey())
	if err != nil {
		t.Fatalf("coefficients: %v", err)
	}
	if cl.Equals(cp) {
		t.Fatal("both halves of a branch key took the same weight, so a chosen punishment key cancels the log key")
	}
}

// The defect itself: L is published in the join before anybody has to name a
// punishment key, so a punisher who announces P = p'G - L collapses a plain sum
// to p'G and takes the bond of somebody who never cheated. The fixture is
// checked before the attack runs, because hand-built point arithmetic that is
// wrong by a sign or a normalize builds "some other point" and every assertion
// below passes under every mutation.
func TestAChosenPunishmentKeyCannotCancelTheLogKey(t *testing.T) {
	log := key(t)
	L := log.PubKey()
	br := Branch{Match: match, Seat: seatOf(t)}
	m := digest("take the bond")

	// The attack as the doc comment describes it: any scalar p', announced as
	// P = p'G - L.
	pPrime := key(t)
	P := addPub(t, pPrime.PubKey(), negPub(L))
	if !addPub(t, L, P).IsEqual(pPrime.PubKey()) {
		t.Fatal("this test did not build the attack it is named for")
	}

	F, err := ForfeitKey(br, L, P)
	if err != nil {
		t.Fatalf("forfeit key: %v", err)
	}
	if F.IsEqual(pPrime.PubKey()) {
		t.Fatal("a chosen punishment key collapsed the branch key to a key the punisher holds alone")
	}
	sig, err := schnorr.Sign(pPrime, m)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if sig.Verify(m, F) {
		t.Fatal("the punisher's chosen scalar signs for the branch key, so it could take a bond from somebody who never cheated")
	}

	// The attacker's best computable candidate. dlog(F) is
	// (c_L - c_P)*l + c_P*p', so everything it can assemble from what it knows
	// is c_P*p' - and the moment the two coefficients collapse into one, that
	// candidate is exactly the branch key.
	_, cp, err := coefficients(br, L, P)
	if err != nil {
		t.Fatalf("coefficients: %v", err)
	}
	best := secp256k1.NewPrivateKey(new(secp256k1.ModNScalar).Mul2(cp, &pPrime.Key))
	if best.PubKey().IsEqual(F) {
		t.Fatal("the punisher's best computable scalar is the branch key, so the two halves are taking one weight")
	}
	bestSig, err := schnorr.Sign(best, m)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if bestSig.Verify(m, F) {
		t.Fatal("the punisher's best computable scalar signs for the branch key")
	}

	// The adaptive attacker: read the coefficients off a stand-in punishment
	// key, solve P1 = cp0^-1 * (zG - cl0*L) for a pinned z, and announce that.
	// If the weights do not really depend on the key they weight, this solve
	// converges exactly and z opens the branch.
	z := privFromHex(t, "4d7f1e02c55a68b03f9a2e11d6c47b88a01e5f3c29d84b7690c2aa5e17f3d940")
	standIn := key(t)
	cl0, cp0, err := coefficients(br, L, standIn.PubKey())
	if err != nil {
		t.Fatalf("coefficients: %v", err)
	}
	inv := new(secp256k1.ModNScalar).Set(cp0).InverseNonConst()
	P1 := mulPub(t, inv, addPub(t, z.PubKey(), negPub(mulPub(t, cl0, L))))
	F1, err := ForfeitKey(br, L, P1)
	if err != nil {
		t.Fatalf("forfeit key: %v", err)
	}
	if F1.IsEqual(z.PubKey()) {
		t.Fatal("a punishment key solved from the coefficients cancelled the log key, so the weights do not depend on the key they weight")
	}
	zSig, err := schnorr.Sign(z, m)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if zSig.Verify(m, F1) {
		t.Fatal("the solved-for scalar signs for the branch key")
	}
}

// The mirror, and it is as bad: an owner free to choose L after seeing P
// announces L = xG - P and reclaims its own bond the block after it confirms,
// with the timelock still weeks away. Both halves are here because plain
// addition satisfies the adaptive half on its own - without the literal half
// the mirror is blind to the very construction this replaces.
func TestAChosenLogKeyCannotCancelThePunishmentKey(t *testing.T) {
	punisher := key(t)
	P := punisher.PubKey()
	br := Branch{Match: match, Seat: seatOf(t)}
	m := digest("reclaim the bond early")

	// A literal x, announced as L = xG - P.
	x := privFromHex(t, "2b8e44a1f0c9d37e615a0b82ddce96f41738c05b9aa2e46d80f715c3ea92cd06")
	L := addPub(t, x.PubKey(), negPub(P))
	if !addPub(t, L, P).IsEqual(x.PubKey()) {
		t.Fatal("this test did not build the attack it is named for")
	}

	F, err := ForfeitKey(br, L, P)
	if err != nil {
		t.Fatalf("forfeit key: %v", err)
	}
	if F.IsEqual(x.PubKey()) {
		t.Fatal("a chosen log key collapsed the branch key to a key the owner holds alone")
	}
	sig, err := schnorr.Sign(x, m)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if sig.Verify(m, F) {
		t.Fatal("the owner's chosen scalar signs for the branch key, so it could reclaim its own bond with the timelock untouched")
	}

	// The owner's best computable candidate: F = c_L*x*G + (c_P - c_L)*P, so
	// everything it holds amounts to c_L*x.
	cl, _, err := coefficients(br, L, P)
	if err != nil {
		t.Fatalf("coefficients: %v", err)
	}
	best := secp256k1.NewPrivateKey(new(secp256k1.ModNScalar).Mul2(cl, &x.Key))
	if best.PubKey().IsEqual(F) {
		t.Fatal("the owner's best computable scalar is the branch key, so the two halves are taking one weight")
	}
	bestSig, err := schnorr.Sign(best, m)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if bestSig.Verify(m, F) {
		t.Fatal("the owner's best computable scalar signs for the branch key")
	}

	// The adaptive owner: read the coefficients off a stand-in log key, solve
	// L1 = cl0^-1 * (zG - cp0*P) for a pinned z, and announce that.
	z := privFromHex(t, "71c05b3da8e92f46013dc7a455f68e9b2064fa81c3b75d92e04a61f8bd23c957")
	standIn := key(t)
	cl0, cp0, err := coefficients(br, standIn.PubKey(), P)
	if err != nil {
		t.Fatalf("coefficients: %v", err)
	}
	inv := new(secp256k1.ModNScalar).Set(cl0).InverseNonConst()
	L1 := mulPub(t, inv, addPub(t, z.PubKey(), negPub(mulPub(t, cp0, P))))
	F1, err := ForfeitKey(br, L1, P)
	if err != nil {
		t.Fatalf("forfeit key: %v", err)
	}
	if F1.IsEqual(z.PubKey()) {
		t.Fatal("a log key solved from the coefficients cancelled the punishment key, so the weights do not depend on the key they weight")
	}
}

// Every element a branch binds has to move the key. A key that survives a
// change of table lets one lawful recovery take two bonds; one that survives a
// change of seat lets a copied announcement stop a table forming; and swapped
// halves are the argument-order mistake that plain addition, being
// commutative, hid completely.
func TestEveryPartOfABranchChangesTheKey(t *testing.T) {
	log, punisher := key(t), key(t)
	seat := seatOf(t)
	br := Branch{Match: match, Seat: seat}

	base, err := ForfeitKey(br, log.PubKey(), punisher.PubKey())
	if err != nil {
		t.Fatalf("forfeit key: %v", err)
	}

	for _, row := range []struct {
		name string
		br   Branch
		l, p *secp256k1.PublicKey
	}{
		{"another table", Branch{Match: otherMatch, Seat: seat}, log.PubKey(), punisher.PubKey()},
		{"another seat", Branch{Match: match, Seat: seatOf(t)}, log.PubKey(), punisher.PubKey()},
		{"another log key", br, key(t).PubKey(), punisher.PubKey()},
		{"another punishment key", br, log.PubKey(), key(t).PubKey()},
		{"the halves swapped", br, punisher.PubKey(), log.PubKey()},
	} {
		t.Run(row.name, func(t *testing.T) {
			alt, err := ForfeitKey(row.br, row.l, row.p)
			if err != nil {
				t.Fatalf("forfeit key: %v", err)
			}
			if alt.IsEqual(base) {
				t.Fatalf("%s left the branch key unchanged, so a branch could be replayed where it does not belong", row.name)
			}
		})
	}

	// The same order mistake on the spending side.
	swapped, err := ForfeitPrivKey(br, punisher, log)
	if err != nil {
		t.Fatalf("forfeit priv: %v", err)
	}
	if swapped.PubKey().IsEqual(base) {
		t.Fatal("a spending key built with its halves swapped still opens the branch, so the two weights are not telling the halves apart")
	}
}

// Degenerate halves are refused rather than combined. Every row is a value a
// caller can really construct, and two of them - one key twice, the key
// negated - weight to keys the bond's owner could spend alone, before the
// timelock.
func TestAForfeitKeyRefusesDegenerateHalves(t *testing.T) {
	log, punisher := key(t), key(t)
	L, P := log.PubKey(), punisher.PubKey()
	seat := seatOf(t)
	br := Branch{Match: match, Seat: seat}

	// NewPublicKey checks nothing, so a point satisfying no curve equation is
	// a value this package can be handed.
	offCurve := func() *secp256k1.PublicKey {
		var x, y secp256k1.FieldVal
		x.SetInt(1)
		y.SetInt(1)
		return secp256k1.NewPublicKey(&x, &y)
	}()

	negL := negPub(L)

	// Shown independently before the table: c_L*L + c_P*(-L) is an ordinary
	// finite point, so the point-at-infinity guard cannot be trusted to catch
	// the negated row - the explicit refusal is load-bearing, not a
	// simplification target.
	cl, cp, err := coefficients(br, L, negL)
	if err != nil {
		t.Fatalf("coefficients: %v", err)
	}
	var ja, jb, wa, wb, sum secp256k1.JacobianPoint
	L.AsJacobian(&ja)
	negL.AsJacobian(&jb)
	secp256k1.ScalarMultNonConst(new(secp256k1.ModNScalar).Set(cl), &ja, &wa)
	secp256k1.ScalarMultNonConst(new(secp256k1.ModNScalar).Set(cp), &jb, &wb)
	secp256k1.AddNonConst(&wa, &wb, &sum)
	sum.ToAffine()
	if (sum.X.IsZero() && sum.Y.IsZero()) || sum.Z.IsZero() {
		t.Fatal("a negated punishment key weighted to the point at infinity, so the infinity guard would have caught it and the explicit refusal is redundant - which is not supposed to be true")
	}

	// 33 bytes shaped exactly like a compressed key, but no point on secp256k1
	// has x = 5.
	notAPoint := make([]byte, 33)
	notAPoint[0] = 0x02
	notAPoint[32] = 0x05

	for _, tc := range []struct {
		name string
		err  func() error
	}{
		{"no log key", func() error { _, err := ForfeitKey(br, nil, P); return err }},
		{"no punishment key", func() error { _, err := ForfeitKey(br, L, nil); return err }},
		{"a log key off the curve", func() error { _, err := ForfeitKey(br, offCurve, P); return err }},
		{"the same key twice", func() error { _, err := ForfeitKey(br, L, L); return err }},
		{"the punishment key negated", func() error { _, err := ForfeitKey(br, L, negL); return err }},
		{"no match", func() error { _, err := ForfeitKey(Branch{Match: "", Seat: seat}, L, P); return err }},
		{"a match of spaces", func() error { _, err := ForfeitKey(Branch{Match: " ", Seat: seat}, L, P); return err }},
		{"a match with a trailing space", func() error { _, err := ForfeitKey(Branch{Match: match + "\n", Seat: seat}, L, P); return err }},
		{"a seat given short", func() error { _, err := ForfeitKey(Branch{Match: match, Seat: seat[:32]}, L, P); return err }},
		// ParsePubKey accepts 65 uncompressed bytes, so this row is the only
		// one the length check alone refuses; the short row above is masked by
		// ParsePubKey and cannot detect a length-check mutation on its own.
		{"a seat given uncompressed", func() error {
			_, err := ForfeitKey(Branch{Match: match, Seat: punisher.PubKey().SerializeUncompressed()}, L, P)
			return err
		}},
		{"a seat that is not a point", func() error { _, err := ForfeitKey(Branch{Match: match, Seat: notAPoint}, L, P); return err }},
		{"a spending key with no recovered half", func() error { _, err := ForfeitPrivKey(br, nil, punisher); return err }},
		{"a spending key with no punisher half", func() error { _, err := ForfeitPrivKey(br, log, nil); return err }},
		{"a spending key from one secret twice", func() error { _, err := ForfeitPrivKey(br, log, log); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err() == nil {
				t.Fatalf("%s still produced a key", tc.name)
			}
		})
	}
}

// The coefficient preimage, pinned byte by byte from literals. Nothing in this
// test comes from running the code under test, so a layout change cannot
// regenerate this vector to match itself - it can only fail here, before it
// strands every branch built under the old layout as a key in no script.
func TestTheBranchCoefficientLayoutIsPinned(t *testing.T) {
	// The keys come from literal scalars rather than literal 33-byte strings,
	// because an arbitrary 33-byte string is overwhelmingly not a parseable
	// point and validate would refuse the seat before the vector was reached.
	lPriv := privFromHex(t, "365ffa9e11c4a0dd7b8c25e9063d51f2b74a88c19e05d637f412bc9a80e5d21c")
	pPriv := privFromHex(t, "58d21f7c0aa39be462f80d1355c6b9827d94ee03ba17f5c4602b8ddca4917f36")
	seatPriv := privFromHex(t, "0d94c3b76f215aee08d3712fc9ba6045e17d2c8b93f0a65d41c8e27b5a90d3f1")
	L, P := lPriv.PubKey(), pPriv.PubKey()
	lBytes := L.SerializeCompressed()
	pBytes := P.SerializeCompressed()
	seat := seatPriv.PubKey().SerializeCompressed()
	br := Branch{Match: match, Seat: seat}

	// The layout: tag raw with no length prefix, the role byte at a fixed
	// offset before anything variable-length, every later field
	// length-prefixed with a big-endian uint32, the derivation counter last.
	preimage := func(role byte) []byte {
		pre := make([]byte, 0, 210)
		pre = append(pre, "dcrpoker/forfeit/keyagg/v1"...) // the tag, raw
		pre = append(pre, role)                            // 0x00 for the log half, 0x01 for the punishment half
		pre = append(pre, 0x00, 0x00, 0x00, 0x40)          // be32(64), the match's length
		pre = append(pre, match...)
		pre = append(pre, 0x00, 0x00, 0x00, 0x21) // be32(33)
		pre = append(pre, lBytes...)
		pre = append(pre, 0x00, 0x00, 0x00, 0x21) // be32(33)
		pre = append(pre, pBytes...)
		pre = append(pre, 0x00, 0x00, 0x00, 0x21) // be32(33)
		pre = append(pre, seat...)
		pre = append(pre, 0x00, 0x00, 0x00, 0x00) // be32(0), the first derivation attempt
		return pre
	}

	pin := func(role byte) *secp256k1.ModNScalar {
		pre := preimage(role)
		if len(pre) != 210 {
			t.Fatalf("the pinned preimage is %d bytes, want 210, so this test's own assembly is wrong", len(pre))
		}
		sum := blake256.Sum256(pre)
		var s secp256k1.ModNScalar
		if overflow := s.SetBytes(&sum); overflow != 0 || s.IsZero() {
			t.Fatal("the pinned preimage hashes past the curve order, so this vector never exercises the first derivation attempt; pick new literals")
		}
		return &s
	}

	wantCL := pin(0x00)
	wantCP := pin(0x01)

	gotCL, err := coefficient(roleLog, br, lBytes, pBytes)
	if err != nil {
		t.Fatalf("coefficient: %v", err)
	}
	if !gotCL.Equals(wantCL) {
		t.Fatal("the log half's coefficient does not match its pinned preimage, so the digest layout has moved and every branch built before the move is a key in no script")
	}
	gotCP, err := coefficient(rolePunish, br, lBytes, pBytes)
	if err != nil {
		t.Fatalf("coefficient: %v", err)
	}
	if !gotCP.Equals(wantCP) {
		t.Fatal("the punishment half's coefficient does not match its pinned preimage, so the digest layout has moved and every branch built before the move is a key in no script")
	}

	// The key itself, derived here from the pinned scalars and nothing else.
	want := addPub(t, mulPub(t, wantCL, L), mulPub(t, wantCP, P))
	got, err := ForfeitKey(br, L, P)
	if err != nil {
		t.Fatalf("forfeit key: %v", err)
	}
	if !got.IsEqual(want) {
		t.Fatal("the branch key is not the pinned coefficients applied to its halves")
	}
	if got.IsEqual(addPub(t, L, P)) {
		t.Fatal("the branch key is the plain sum of its halves, so the weighting is not happening at all")
	}
}

// The availability pair to the refusals above: a branch is needed twice,
// months apart, and Branch.Seat aliases the caller's slice. A branch rebuilt
// from copies of its parts must give the same key after the original buffer
// has been reused, or a bond built today cannot be spent at recovery time.
func TestABranchRebuiltFromItsPartsGivesTheSameKey(t *testing.T) {
	log, punisher := key(t), key(t)
	seat := seatOf(t)

	forfeit, err := ForfeitKey(Branch{Match: match, Seat: seat}, log.PubKey(), punisher.PubKey())
	if err != nil {
		t.Fatalf("forfeit key: %v", err)
	}

	// Rebuild the branch from copies, then clobber the original buffer, which
	// is what a caller's I/O layer will eventually do to it.
	rebuilt := Branch{
		Match: string(append([]byte(nil), match...)),
		Seat:  append([]byte(nil), seat...),
	}
	for i := range seat {
		seat[i] = 0xa5
	}

	again, err := ForfeitKey(rebuilt, log.PubKey(), punisher.PubKey())
	if err != nil {
		t.Fatalf("forfeit key from the rebuilt branch: %v", err)
	}
	if !again.IsEqual(forfeit) {
		t.Fatal("a branch rebuilt from copies of its parts derived a different key, so a bond built today could not be spent months from now")
	}
	spend, err := ForfeitPrivKey(rebuilt, log, punisher)
	if err != nil {
		t.Fatalf("forfeit priv: %v", err)
	}
	if !spend.PubKey().IsEqual(forfeit) {
		t.Fatal("the spending key from the rebuilt branch does not open the branch the bond was built with")
	}
}

// End to end: a player equivocates, the other one ends up holding the key to
// the punishment branch, and nobody else does.
func TestEquivocationHandsTheBondToTheWrongedPlayer(t *testing.T) {
	cheat, err := NewLogKey(match)
	if err != nil {
		t.Fatalf("log key: %v", err)
	}
	punisher, err := PunishmentKey()
	if err != nil {
		t.Fatalf("punishment key: %v", err)
	}

	br := Branch{Match: match, Seat: seatOf(t)}
	forfeit, err := ForfeitKey(br, cheat.Public(), punisher.PubKey())
	if err != nil {
		t.Fatalf("forfeit key: %v", err)
	}

	// Before any cheating, the wronged player cannot spend the branch: they
	// have one half and no way to the other.
	if _, err := Recover(cheat.Public(), digest("a"), make([]byte, 64), digest("b"), make([]byte, 64)); err == nil {
		t.Fatal("a key was recovered from nothing")
	}

	// The cheat tells one player it folded and another that it raised. It
	// signs through the primitive rather than the LogKey, because the book on
	// the key refuses this - and a cheat runs its own software, so the guard
	// protects an honest caller from a mistake and never a cheat from itself.
	folded, raised := digest("seat 0 folds"), digest("seat 0 raises 2000")
	at := Position{Match: cheat.Match(), Domain: DomainEntry, Seq: 11}
	a, err := Sign(cheat.priv, at, folded)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	b, err := Sign(cheat.priv, at, raised)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	// Either recipient can now compute the cheat's log key...
	leaked, err := Recover(cheat.Public(), folded, a, raised, b)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}

	// ...but only the player holding the punishment key can spend the bond,
	// and only by rebuilding the same branch the bond was built from.
	spend, err := ForfeitPrivKey(br, leaked, punisher)
	if err != nil {
		t.Fatalf("forfeit priv: %v", err)
	}
	if !spend.PubKey().IsEqual(forfeit) {
		t.Fatal("the wronged player cannot spend the punishment branch")
	}

	// A bystander who saw the same equivocation holds the leaked key and
	// their own key, and that combination is not the branch.
	bystander, err := PunishmentKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	theirs, err := ForfeitPrivKey(br, leaked, bystander)
	if err != nil {
		t.Fatalf("forfeit priv: %v", err)
	}
	if theirs.PubKey().IsEqual(forfeit) {
		t.Fatal("a bystander could spend the punishment branch")
	}
}

// The nonce preimages, pinned byte by byte from literals. Nothing here comes
// from running the code under test, so a derivation change cannot regenerate
// these vectors to match itself - it can only fail here. The stakes are not a
// broken signature but a disabled punishment: Recover only fires when two
// signatures at one position share r, so a seat signing under an old and a new
// derivation at the same position would equivocate unpunished.
func TestTheNoncePreimageLayoutIsPinned(t *testing.T) {
	const keyHex = "6c81d5a9f3072b4e88c1d02e5f9a3b76014d8c2ea9b5f7301e6d4a8c2f5b9d13"
	keyBytes, err := hex.DecodeString(keyHex)
	if err != nil || len(keyBytes) != 32 {
		t.Fatalf("the literal %q is not a 32-byte scalar", keyHex)
	}
	priv := privFromHex(t, keyHex)

	// The layout: tag raw with no length prefix, then the key, the match and
	// the domain each be32-length-prefixed, the sequence as a raw big-endian
	// uint64, the derivation counter last as a raw big-endian uint32.
	pre := make([]byte, 0, 150)
	pre = append(pre, "dcrpoker/forfeit/nonce/v1"...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x20) // be32(32), the key's length
	pre = append(pre, keyBytes...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x40) // be32(64), the match's length
	pre = append(pre, match...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x05) // be32(5), the domain's length
	pre = append(pre, "entry"...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07) // be64(7), the sequence
	pre = append(pre, 0x00, 0x00, 0x00, 0x00)                         // be32(0), the first derivation attempt
	if len(pre) != 150 {
		t.Fatalf("the pinned preimage is %d bytes, want 150, so this test's own assembly is wrong", len(pre))
	}

	sum := blake256.Sum256(pre)
	var want secp256k1.ModNScalar
	if overflow := want.SetBytes(&sum); overflow != 0 || want.IsZero() {
		t.Fatal("the pinned preimage hashes past the curve order, so this vector never exercises the first derivation attempt; pick new literals")
	}

	got, err := nonce(priv, Position{Match: match, Domain: DomainEntry, Seq: 7})
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}
	if !got.Equals(&want) {
		t.Fatal("the nonce does not match its pinned preimage, so the derivation has moved and equivocation across the change goes unpunished")
	}
}

func TestTheCommittedNoncePreimageLayoutIsPinned(t *testing.T) {
	const keyHex = "6c81d5a9f3072b4e88c1d02e5f9a3b76014d8c2ea9b5f7301e6d4a8c2f5b9d13"
	const hashHex = "9e2f4c6a815d3b70e4f8a2c95d1b6380f7e3a9c14b6d28f05a9c3e17d4b86f20"
	keyBytes, err := hex.DecodeString(keyHex)
	if err != nil || len(keyBytes) != 32 {
		t.Fatalf("the literal %q is not a 32-byte scalar", keyHex)
	}
	msgHash, err := hex.DecodeString(hashHex)
	if err != nil || len(msgHash) != 32 {
		t.Fatalf("the literal %q is not a 32-byte hash", hashHex)
	}
	priv := privFromHex(t, keyHex)

	// The committed layout is the position layout with its own tag and the
	// message hash inserted, be32-length-prefixed, between sequence and counter.
	pre := make([]byte, 0, 198)
	pre = append(pre, "dcrpoker/forfeit/nonce-committed/v1"...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x20) // be32(32), the key's length
	pre = append(pre, keyBytes...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x40) // be32(64), the match's length
	pre = append(pre, match...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x07) // be32(7), the domain's length
	pre = append(pre, "shuffle"...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03) // be64(3), the sequence
	pre = append(pre, 0x00, 0x00, 0x00, 0x20)                         // be32(32), the message hash's length
	pre = append(pre, msgHash...)
	pre = append(pre, 0x00, 0x00, 0x00, 0x00) // be32(0), the first derivation attempt
	if len(pre) != 198 {
		t.Fatalf("the pinned preimage is %d bytes, want 198, so this test's own assembly is wrong", len(pre))
	}

	sum := blake256.Sum256(pre)
	var want secp256k1.ModNScalar
	if overflow := want.SetBytes(&sum); overflow != 0 || want.IsZero() {
		t.Fatal("the pinned preimage hashes past the curve order, so this vector never exercises the first derivation attempt; pick new literals")
	}

	got, err := committedNonce(priv, Position{Match: match, Domain: DomainShuffle, Seq: 3}, msgHash)
	if err != nil {
		t.Fatalf("committed nonce: %v", err)
	}
	if !got.Equals(&want) {
		t.Fatal("the committed nonce does not match its pinned preimage, so the derivation has moved and equivocation across the change goes unpunished")
	}
}
