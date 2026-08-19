// Package ruling is how a game tells the runtime that a seat has forfeited.
//
// The split this package exists to hold is that a game decides *that* someone
// forfeited and the runtime decides *how the money moves*. Deciding is game
// logic and nothing else: what counts as cheating at poker looks nothing like
// what counts as cheating at battleships, the evidence is different, and the
// referee that weighs it is the part of a game nobody can write for anyone
// else. Moving the money is not game logic at all. The same small set of escrow
// branches carries every game's punishment, the transactions are already built
// by the escrow package, and getting one wrong costs a player their bond.
//
// So a ruling names a seat, a kind, and its proof. It never names a
// transaction, a script, an address or an amount, and there is a test that
// fails if anyone adds a field that does. The runtime derives all of those from
// the table's own terms, which is what makes it impossible for a ruling to
// direct money anywhere - the worst a wrong ruling can do is punish the wrong
// seat of the same table, and even that is bounded by what the proof supports.
//
// # Why two kinds need different guarantees
//
// A ruling is checked where checking is possible, and where it is not, the
// mechanism is built so that checking is unnecessary.
//
// Equivocation is checked, cryptographically, here. Two signatures at one
// position that share a nonce expose the signer's key; that is arithmetic, and
// forfeit.Recover either produces the key or reports that no key is exposed. A
// game cannot cause a bond to be swept by asserting that someone cheated,
// because the assertion is not what moves the money - the recovered key is, and
// only a real equivocation yields one.
//
// Silence is not checked, and does not need to be. Whether a duty was owed at
// all is game logic the SDK has no vocabulary for, so the runtime takes the
// game's word - but the punishment path for silence is the claim ladder, and
// the ladder gives the accused an on-chain right of reply. An accusation
// against a seat that is in fact alive is answered, the ladder runs out, and
// the accuser has bought nothing but attrition. The safety comes from the
// mechanism rather than from the SDK believing anybody.
//
// Clean is the absence of a ruling in all but name: the bond is released, and
// there is nothing to prove.
//
// # Why the duty is an opaque string
//
// A silence ruling names its duty as a label the game chooses. It would be
// convenient to reuse schema.DutyKind, and that would be a mistake: those
// values are cardkey, shuffle, share, action, checkpoint and reveal, and
// schema.Duty carries a hand number. That is poker's vocabulary, inherited
// verbatim when this module was extracted from dcrpoker, and a battleships
// forfeiture has no hand and shuffles nothing. The runtime never interprets the
// label; it carries it into the log and back out so a person reading an
// accusation can tell what was owed.
package ruling

import (
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
)

// Kind names the punishment path a ruling selects.
//
// There are three because there are three ways a bond can leave an escrow, not
// because three read well: swept by the victim of an equivocation, taken by a
// wronged seat after an unanswered claim, or released because nothing went
// wrong. A game that believes it needs a fourth has found game logic, not a
// missing branch.
type Kind uint8

const (
	// Clean releases the bond to its owner. No proof, no fault.
	Clean Kind = iota
	// Equivocation sweeps the forfeitable bond to the seat that was lied
	// to. Proven here, before anything is spent.
	Equivocation
	// Silence runs the claim ladder against a seat the game says stopped
	// answering. The accused can answer on chain.
	Silence
)

func (k Kind) String() string {
	switch k {
	case Clean:
		return "clean"
	case Equivocation:
		return "equivocation"
	case Silence:
		return "silence"
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// Ruling is a game's decision that one seat of one table has forfeited.
//
// Exactly one proof field is set, and it is the one the kind names. Validate
// enforces that; the runtime calls it before it looks at anything else.
type Ruling struct {
	// Match is the table the ruling belongs to.
	Match string
	// Against is the seat being ruled against.
	Against uint32
	// Kind selects the punishment path and says which proof must be set.
	Kind Kind

	// Equivocated is set if and only if Kind is Equivocation.
	Equivocated *Equivocated
	// Silent is set if and only if Kind is Silence.
	Silent *Silent
}

// Equivocated is the proof that a seat signed two different statements at one
// position, which is what publishes its key.
//
// Both signatures are individually valid - that is what makes the cheat work at
// all, since each recipient sees a perfectly good signature. What gives it away
// is that they share a nonce, and sharing a nonce is what the arithmetic in
// forfeit.Recover turns back into the private key.
type Equivocated struct {
	// At is the position both statements were signed at. A pair signed at
	// two different positions is two honest signatures, not a cheat.
	At forfeit.Position
	// Pub is the compressed session key that signed both.
	Pub []byte

	// HashA and SigA, HashB and SigB are the two signed statements.
	HashA []byte
	SigA  []byte
	HashB []byte
	SigB  []byte
}

// Silent is a game's finding that a seat owed something and did not do it.
//
// The runtime does not check this and cannot: what was owed is game logic. The
// ladder is what makes that safe, by letting the accused answer.
type Silent struct {
	// Duty is the game's own label for what went undone. Carried, never
	// interpreted.
	Duty string
	// Seq is the position in the game's log the duty sat at.
	Seq uint64
	// By is the block height the duty lapsed at. The runtime refuses to
	// accuse before the chain tip has passed it.
	By uint32
}

// Validate reports whether the ruling is structurally answerable, before any
// proof is weighed and long before anything is spent.
func (r Ruling) Validate() error {
	if r.Match == "" {
		return fmt.Errorf("a ruling needs a match to belong to")
	}
	switch r.Kind {
	case Clean:
		if r.Equivocated != nil || r.Silent != nil {
			return fmt.Errorf("a clean ruling carries no proof, and this one does")
		}
	case Equivocation:
		if r.Equivocated == nil {
			return fmt.Errorf("an equivocation ruling without its proof is an accusation")
		}
		if r.Silent != nil {
			return fmt.Errorf("a ruling carries one proof, and this one names two")
		}
		return r.Equivocated.validate()
	case Silence:
		if r.Silent == nil {
			return fmt.Errorf("a silence ruling must name the duty that went undone")
		}
		if r.Equivocated != nil {
			return fmt.Errorf("a ruling carries one proof, and this one names two")
		}
		return r.Silent.validate()
	default:
		return fmt.Errorf("no such ruling kind: %d", uint8(r.Kind))
	}
	return nil
}

func (e Equivocated) validate() error {
	if len(e.Pub) == 0 {
		return fmt.Errorf("an equivocation names the key that signed twice")
	}
	if len(e.HashA) != 32 || len(e.HashB) != 32 {
		return fmt.Errorf("both signed statements must be 32-byte digests")
	}
	if len(e.SigA) != forfeit.SigLen || len(e.SigB) != forfeit.SigLen {
		return fmt.Errorf("both signatures must be %d bytes", forfeit.SigLen)
	}
	if string(e.HashA) == string(e.HashB) {
		return fmt.Errorf("the same statement signed twice is not equivocation")
	}
	return nil
}

func (s Silent) validate() error {
	if s.Duty == "" {
		return fmt.Errorf("a silence ruling must say what was owed")
	}
	if s.By == 0 {
		return fmt.Errorf("a silence ruling must say what height the duty lapsed at")
	}
	return nil
}

// Recover returns the key the equivocation exposed.
//
// This is the whole guarantee of an equivocation ruling: the game's claim is
// not what moves the bond, the recovered key is, and only a genuine pair of
// signatures sharing a nonce yields one. A game that rules wrongly - or lies -
// gets an error here and no transaction.
func (e Equivocated) Recover() (*secp256k1.PrivateKey, error) {
	if err := e.validate(); err != nil {
		return nil, err
	}
	pub, err := secp256k1.ParsePubKey(e.Pub)
	if err != nil {
		return nil, fmt.Errorf("parse the key that signed twice: %w", err)
	}
	// forfeit.Recover refuses a pair whose recovered key is not the named
	// one, so a ruling cannot borrow another seat's equivocation.
	return forfeit.Recover(pub, e.HashA, e.SigA, e.HashB, e.SigB)
}
