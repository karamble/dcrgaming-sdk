package forfeit

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// LogKey is a per-match key whose only job is signing the log.
//
// Separate from the session key on purpose, and the separation is the whole
// safety argument. The session key is named in the escrow script and holds the
// stake; if it were the key that leaked, an equivocating player would not be
// forfeiting a bond to the player they cheated - they would be publishing the
// key to their own stake, for anyone at all to take first. The penalty has to
// be bounded and it has to be directed at the person wronged.
//
// One per match, and never reused across matches: the key is expected to become
// public the moment its owner misbehaves, and a key reused at another table
// would take that table down with it.
type LogKey struct {
	priv  *secp256k1.PrivateKey
	match string

	// book is what this key has signed and where, so a second, different
	// message at one position is refused instead of publishing the key. That
	// mistake is reachable by accident: a table whose hand counter starts
	// over signs hand one again with a different deck.
	mu   sync.Mutex
	book Book
}

// Book remembers what a key signed at each position. One that does not outlive
// the process is a key with no memory of what it has already put its name to.
//
// A Record that fails means the position was not remembered, and a signature
// over an unremembered position is exactly what the book exists to prevent - so
// the failure refuses the signature rather than being noted somewhere.
type Book interface {
	Used(p Position) ([32]byte, bool)
	Record(p Position, digest [32]byte) error
}

// memoryBook catches the mistake within one process, and no further.
type memoryBook struct{ at map[Position][32]byte }

func (b *memoryBook) Used(p Position) ([32]byte, bool) {
	d, ok := b.at[p]
	return d, ok
}

func (b *memoryBook) Record(p Position, digest [32]byte) error {
	if b.at == nil {
		b.at = map[Position][32]byte{}
	}
	b.at[p] = digest
	return nil
}

// Remember gives this key a book that outlives the process.
func (k *LogKey) Remember(b Book) {
	if b == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.book = b
}

// NewLogKey draws a log key for one match.
func NewLogKey(match string) (*LogKey, error) {
	if match == "" {
		return nil, fmt.Errorf("a log key needs a match to belong to")
	}
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	return &LogKey{priv: priv, match: match}, nil
}

// LogKeyFrom wraps an existing private key, for restoring one from storage.
func LogKeyFrom(priv *secp256k1.PrivateKey, match string) (*LogKey, error) {
	if priv == nil {
		return nil, fmt.Errorf("no key")
	}
	if match == "" {
		return nil, fmt.Errorf("a log key needs a match to belong to")
	}
	return &LogKey{priv: priv, match: match}, nil
}

// Public is the key others verify against and build the forfeit key from.
func (k *LogKey) Public() *secp256k1.PublicKey { return k.priv.PubKey() }

// Match is the match this key belongs to.
func (k *LogKey) Match() string { return k.match }

// Sign signs one message at one position in this match's log.
//
// Calling it twice at one position with different messages publishes the key.
// That is not a hazard to be guarded against here - it is what the key is for -
// but it does mean a caller must be sure that a position means one message. The
// sequence number comes from the chain, which enforces exactly that.
func (k *LogKey) Sign(d Domain, seq uint64, hash []byte) ([]byte, error) {
	p := Position{Match: k.match, Domain: d, Seq: seq}
	if len(hash) != 32 {
		return nil, fmt.Errorf("message digest is %d bytes, want 32", len(hash))
	}
	var digest [32]byte
	copy(digest[:], hash)

	k.mu.Lock()
	defer k.mu.Unlock()
	if k.book == nil {
		k.book = &memoryBook{}
	}
	if was, ok := k.book.Used(p); ok && was != digest {
		return nil, fmt.Errorf("%s/%d was already signed over a different message; "+
			"signing it again would publish this key", d, seq)
	}
	// Recorded before it is signed, never after. A failure between the two
	// leaves a position remembered and nothing signed, which costs nothing:
	// the same digest may be signed again. The other order can return a
	// signature the book never heard of.
	if err := k.book.Record(p, digest); err != nil {
		return nil, fmt.Errorf("%s/%d cannot be recorded, so it will not be signed: %w", d, seq, err)
	}
	return Sign(k.priv, p, hash)
}

// SignCommitted signs content its position does not determine. Two different
// messages at one position are two signatures and no disclosure.
func (k *LogKey) SignCommitted(d Domain, seq uint64, hash []byte) ([]byte, error) {
	return SignCommitted(k.priv, Position{Match: k.match, Domain: d, Seq: seq}, hash)
}

// pkg/forfeit imports no first-party package, and pkg/escrow's own internal
// tests depend on that - escrow.PubKeyLen here would make the bridge test an
// import cycle.
const pubKeyLen = 33

var keyAggTag = []byte("dcrpoker/forfeit/keyagg/v1")

const (
	roleLog    byte = 0x00
	rolePunish byte = 0x01
)

// Branch names one punishment branch: the table it belongs to and the seat it
// is aimed at.
//
// Both halves of a branch key are weighted by a hash of these values, so a
// branch cannot be lifted to a table where the same keys would mean something
// else, and two seats that announce the same punishment key get two different
// branch keys rather than one duplicate the bond builder has to refuse - which
// would stop the table forming and cost the seat that copied it nothing.
//
// The seat is the punisher's compressed session key, because that is what the
// roster records for a seat and what the escrow scripts name.
//
// It is carried as a unit because the same values are needed twice, months
// apart: once to build the bond and once to spend it. They are hashed exactly
// as given - not trimmed, not lowercased - and Seat is read at hash time rather
// than copied, so a Branch must not be mutated in between. A branch rebuilt
// wrongly produces a key that is in no script, which is the question
// escrow.ForfeitIndex answers before anything is broadcast.
type Branch struct {
	Match string // the table this branch belongs to
	Seat  []byte // the punisher's compressed session key
}

func (br Branch) validate() error {
	if strings.TrimSpace(br.Match) == "" {
		return fmt.Errorf("a branch needs a match to belong to")
	}
	if br.Match != strings.TrimSpace(br.Match) {
		return fmt.Errorf("a branch's match has surrounding space, and it is hashed exactly as given")
	}
	if len(br.Seat) != pubKeyLen {
		return fmt.Errorf("a branch's seat is %d bytes, want a %d byte compressed key",
			len(br.Seat), pubKeyLen)
	}
	if _, err := secp256k1.ParsePubKey(br.Seat); err != nil {
		return fmt.Errorf("a branch's seat: %w", err)
	}
	return nil
}

// ForfeitKey is the public key a bond's punishment branch pays to.
//
//	F = c_L*L + c_P*P
//
// where L is the cheat's log key, P is the wronged player's own punishment key,
// and the two coefficients are hashes of the branch and of both keys together.
//
// The plain sum L + P is the obvious construction and it does not work. L is
// published in the join before anybody has to name a punishment key, so the
// other player picks any scalar p' and announces P = p'G - L. The sum collapses
// to p'G, a key they alone hold, and they take the bond of somebody who has not
// cheated - no equivocation, no secret, only arithmetic on published points.
// The mirror is as bad and points the other way: an owner free to choose L
// after seeing P announces L = xG - P and reclaims its own bond the moment it
// confirms, with the timelock still a week away. Neither direction is caught by
// checking that the keys differ or that they do not sum to infinity, because
// the rogue key is a perfectly ordinary point.
//
// Weighting each half by a coefficient that depends on both halves closes both
// directions at once. The coefficient is a hash of the choice, so the choice
// cannot be made to depend on it: cancelling a half means hitting a fixed point
// of the hash blind. What is left is the property the plain sum only appeared
// to have:
//
//   - The bond's owner knows the secret behind L and cannot open this key
//     without the secret behind P.
//   - The other player knows the secret behind P and cannot open this key
//     without the secret behind L, so they cannot take a bond from somebody who
//     has not cheated. There is no accusation to make and nothing to grief.
//
// Both of those are statements about this key and neither is a statement about
// the bond. An owner that also controls the seat which announced P holds both
// halves of that seat's branch and can take its own bond back whenever it
// likes. A branch is worth the independence of the seat it names, and nothing
// here can establish that.
//
// The moment the owner equivocates, anyone holding both of its conflicting
// signatures can compute L, and the player whose branch it is holds the other
// half. This is Lightning's revocation trick, doing here what it does there:
// turning "prove they cheated" into "they handed you the key".
//
// One branch per opponent, because the punishment must be directed. A branch
// keyed to L alone would be spendable by any bystander who noticed.
//
// What this does not settle is who announced P. A weighting is not a signature
// and this function never sees one; the announcement has to be authenticated by
// the punisher's session key, for the same reason the log key is. A branch is
// worth exactly what the message that carried its key was worth.
//
// A seat that announces the log key itself, or its negation, gets no branch:
// this returns an error and the caller builds the bond from the remaining
// seats. Refusing to build the bond at all would let any seat stop a table
// forming with two published bytes and no secret.
func ForfeitKey(br Branch, logPub, punisherPub *secp256k1.PublicKey) (*secp256k1.PublicKey, error) {
	if logPub == nil || punisherPub == nil {
		return nil, fmt.Errorf("a forfeit key needs both halves")
	}
	// NewPublicKey does not check the curve and this function returns one, so
	// an off-curve key is a value this package can hand itself.
	if !logPub.IsOnCurve() || !punisherPub.IsOnCurve() {
		return nil, fmt.Errorf("a forfeit key needs two points on the curve")
	}
	if err := br.validate(); err != nil {
		return nil, err
	}
	l := logPub.SerializeCompressed()
	p := punisherPub.SerializeCompressed()
	if logPub.IsEqual(punisherPub) {
		return nil, fmt.Errorf("a forfeit key cannot be built from one key twice; " +
			"the bond's owner would be able to spend it alone")
	}
	// P = -L weights to (c_L - c_P)*L, which the owner can sign for on its own.
	// Plain addition sent this to the point at infinity and the guard below
	// caught it by accident; weighting does not, so it is named here.
	if l[0] != p[0] && bytes.Equal(l[1:], p[1:]) {
		return nil, fmt.Errorf("the punishment key is the log key negated, " +
			"so the bond's owner could take their own bond back early")
	}
	cl, cp, err := coefficients(br, logPub, punisherPub)
	if err != nil {
		return nil, err
	}
	var a, b, wa, wb, sum secp256k1.JacobianPoint
	logPub.AsJacobian(&a)
	punisherPub.AsJacobian(&b)
	secp256k1.ScalarMultNonConst(cl, &a, &wa)
	secp256k1.ScalarMultNonConst(cp, &b, &wb)
	secp256k1.AddNonConst(&wa, &wb, &sum)
	sum.ToAffine()
	// Unreachable past the two checks above: a weighted sum lands on infinity
	// only at a fixed point of its own coefficients. Kept as the last word.
	if (sum.X.IsZero() && sum.Y.IsZero()) || sum.Z.IsZero() {
		return nil, fmt.Errorf("these keys weight to the point at infinity")
	}
	return secp256k1.NewPublicKey(&sum.X, &sum.Y), nil
}

func coefficients(br Branch, logPub, punisherPub *secp256k1.PublicKey) (cl, cp *secp256k1.ModNScalar, err error) {
	l := logPub.SerializeCompressed()
	p := punisherPub.SerializeCompressed()
	cl, err = coefficient(roleLog, br, l, p)
	if err != nil {
		return nil, nil, err
	}
	cp, err = coefficient(rolePunish, br, l, p)
	if err != nil {
		return nil, nil, err
	}
	return cl, cp, nil
}

// coefficient is the weight one half of a branch key takes.
//
// The layout is fixed and explicit rather than produced by an encoder. Two
// implementations that disagree by one byte derive two different keys, and the
// disagreement shows up as a bond nobody can spend rather than as an error. The
// match is hashed exactly as given for the same reason - a branch whose match
// is not already trimmed is refused rather than trimmed here, because two peers
// that normalized differently would each be sure they were right.
//
// The role byte is the whole difference between the two halves. Without it both
// take the same weight, F collapses to c*(L+P), and a punisher who announces
// P = p'G - L holds c*p' - which is the attack this construction exists to
// stop, back again with one byte missing.
//
// The counter only ever advances past a coefficient that is zero or over the
// curve order. Zero is the case worth naming: c_L = 0 would make F = c_P*P,
// which the punisher could spend without anyone having cheated, and c_P = 0
// hands the branch to the owner. Both are 2^-256 events and both are the whole
// scheme silently becoming no scheme.
func coefficient(role byte, br Branch, logPub, punisherPub []byte) (*secp256k1.ModNScalar, error) {
	for i := uint32(0); i < 8; i++ {
		h := blake256.New()
		h.Write(keyAggTag)
		h.Write([]byte{role})
		writeField(h, []byte(br.Match))
		writeField(h, logPub)
		writeField(h, punisherPub)
		writeField(h, br.Seat)
		_ = binary.Write(h, binary.BigEndian, i)

		var raw [32]byte
		copy(raw[:], h.Sum(nil))
		var c secp256k1.ModNScalar
		if overflow := c.SetBytes(&raw); overflow == 0 && !c.IsZero() {
			return &c, nil
		}
	}
	return nil, fmt.Errorf("could not derive a branch coefficient for match %s", br.Match)
}

// ForfeitPrivKey is the spending key for a punishment branch, once the cheat's
// log key has been recovered from their own equivocation.
//
//	d = c_L*l + c_P*p
//
// The branch has to be the same one the bond was built from, and it is not
// checked here: recomputing the branch key from these two secrets would agree
// with itself whatever branch was passed in. The check that means anything is
// escrow.ForfeitIndex against the bond on chain, which the caller runs anyway
// to learn which branch to spend - and a branch rebuilt wrongly fails there, as
// "this bond has no punishment branch for that key", rather than as a
// transaction that never confirms.
func ForfeitPrivKey(br Branch, recovered, punisher *secp256k1.PrivateKey) (*secp256k1.PrivateKey, error) {
	if recovered == nil || punisher == nil {
		return nil, fmt.Errorf("a forfeit key needs both halves")
	}
	// Through ForfeitKey so the halves are refused here for exactly the
	// reasons they are refused there, and the two can never drift apart.
	if _, err := ForfeitKey(br, recovered.PubKey(), punisher.PubKey()); err != nil {
		return nil, err
	}
	cl, cp, err := coefficients(br, recovered.PubKey(), punisher.PubKey())
	if err != nil {
		return nil, err
	}
	d := new(secp256k1.ModNScalar).Mul2(cl, &recovered.Key)
	d.Add(new(secp256k1.ModNScalar).Mul2(cp, &punisher.Key))
	// Unreachable: d = 0 means F is the point at infinity, which ForfeitKey
	// has already refused. Kept for the same reason that guard is.
	if d.IsZero() {
		return nil, fmt.Errorf("these keys weight to zero")
	}
	return secp256k1.NewPrivateKey(d), nil
}

// PunishmentKey draws a fresh key for punishing one opponent at one match.
//
// One per opponent per match. Reuse across opponents would let one cheat's
// leaked key be combined with a punishment key another branch also names, which
// is not exploitable today but is the kind of sharing that becomes one later.
func PunishmentKey() (*secp256k1.PrivateKey, error) {
	return secp256k1.GeneratePrivateKey()
}
