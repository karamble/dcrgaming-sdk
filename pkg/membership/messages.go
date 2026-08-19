package membership

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
)

// SigLen is the length of a consensus Schnorr signature. Nothing here is
// spending an output, so there is no trailing hash-type byte.
const SigLen = 64

// Domain separation, so a signature made for one purpose can never be read as
// another. Without it a join could be replayed as a commit, or either replayed
// at a different table by the same key.
var (
	termsTag  = []byte("gaming/table/terms/v1")
	joinTag   = []byte("gaming/table/join/v1")
	rosterTag = []byte("gaming/table/roster/v1")
	assertTag = []byte("gaming/table/assert/v1")
	seatTag   = []byte("gaming/table/seats/v1")
	commitTag = []byte("gaming/table/commit/v1")
	fundedTag = []byte("gaming/table/funded/v1")
	// bondedTag separates a bond announcement from a stake announcement. The
	// two carry the same fields, so without it one could be replayed as the
	// other - and a seat's bond passed off as its stake would have a table
	// dealing against money that is not there.
	bondedTag = []byte("gaming/table/bonded/v1")
	payoutTag = []byte("gaming/table/payout/v1")
	// bondTermsTag is additive: a new hash input, never a change to a frozen one.
	bondTermsTag = []byte("gaming/table/bond-terms/v1")
)

// Terms are what an invitation states and every join commits to.
//
// They are signed rather than merely agreed because an invitation is ordinary
// chat text, so whoever forwards it can hand one player one buy-in and another
// a different one. Winner-take-all only divides a pot fairly across equal
// stakes, so two players who read different invitations must fail to form a
// table rather than form one and discover it at settlement.
type Terms struct {
	Game       string
	GameVer    int
	SID        string
	BuyInAtoms uint64
	Seats      uint32
	// CSVBlocks is the relative timelock on every member's refund branch.
	// It is in the terms because each member builds their own branch, so a
	// member whose refund matured in one block could pull their stake
	// mid-hand; everyone has to be able to check everyone else's.
	CSVBlocks uint32
	// Until is the block height after which no more joins are admitted.
	//
	// It is a height rather than a time because it has to be a fact every
	// peer can check and nobody can be shown to have read wrong. Clocks
	// disagree, and a deadline each machine reads differently is one that
	// decides membership by whose clock ran fast.
	Until uint32

	// BondAtoms and BondLockBlocks are the table's bond terms: what each
	// seat posts, and the relative lock it sits behind.
	//
	// They are here for the reason CSVBlocks is: every seat builds its own
	// bond, so every seat has to be able to check everyone else's, and terms
	// nobody agreed are terms nobody can check. Games differ on the values -
	// battleships locks bonds for 4032 blocks where the escrow floor is 2016
	// - so the floors live in escrow and the choice lives here.
	//
	// Both zero means the table has no bonds, which is a table poker formed
	// for years. See Hash for why that case must stay byte-identical.
	BondAtoms      uint64
	BondLockBlocks uint32
}

// Validate reports whether the terms could describe a table at all.
//
// The session id is checked only for being present. What shape it must take is
// the wire's business, and an invitation that carries a bad one should be
// refused when it is read rather than here.
func (t Terms) Validate() error {
	if strings.TrimSpace(t.Game) == "" {
		return fmt.Errorf("terms name no game")
	}
	if strings.TrimSpace(t.SID) == "" {
		return fmt.Errorf("terms name no session")
	}
	if t.Seats < 2 || int(t.Seats) > escrow.MaxMembers {
		return fmt.Errorf("a table of %d seats is outside 2..%d", t.Seats, escrow.MaxMembers)
	}
	if t.CSVBlocks == 0 {
		return fmt.Errorf("terms state no refund timelock")
	}
	if t.CSVBlocks > escrow.MaxCSVBlocks {
		// A refund spends by putting this number in the input's sequence,
		// and consensus reads only the low 16 bits of it. A larger lock
		// builds a script whose refund branch can never be satisfied by
		// anybody - so the money would go in and never come out. Refuse
		// the invitation rather than fund a table nobody can leave.
		return fmt.Errorf("a refund timelock of %d blocks is beyond %d, and could never be spent",
			t.CSVBlocks, escrow.MaxCSVBlocks)
	}
	if t.Until == 0 {
		// Without a deadline there is no moment at which "no more joins
		// are coming" becomes a fact, and a table can only ever be
		// formed by guessing.
		return fmt.Errorf("terms state no admission deadline")
	}
	if t.bonded() {
		// Stating one term and not the other is caught by the floors
		// below: the missing half is zero, and zero is under both.
		if t.BondAtoms < escrow.MinBondAtoms {
			return fmt.Errorf("a bond of %d atoms is under the escrow floor of %d",
				t.BondAtoms, escrow.MinBondAtoms)
		}
		if t.BondLockBlocks < escrow.MinBondBlocks {
			return fmt.Errorf("a bond lock of %d blocks is under the escrow floor of %d",
				t.BondLockBlocks, escrow.MinBondBlocks)
		}
	}
	return nil
}

// Hash is the digest joins and commits bind to.
//
// Bond terms are appended only when the table has them, and behind their own
// tag. That is not tidiness: dcrpoker's live tables carry joins and commits
// signed over this digest, and a table without bond terms has to keep hashing
// exactly the bytes it did before they existed or every one of those signatures
// stops verifying. A table with bond terms is a table nobody has signed yet.
func (t Terms) Hash() ([32]byte, error) {
	if err := t.Validate(); err != nil {
		return [32]byte{}, err
	}
	var b bytes.Buffer
	b.Write(termsTag)
	writeString(&b, t.Game)
	_ = binary.Write(&b, binary.BigEndian, int64(t.GameVer))
	writeString(&b, t.SID)
	_ = binary.Write(&b, binary.BigEndian, t.BuyInAtoms)
	_ = binary.Write(&b, binary.BigEndian, t.Seats)
	_ = binary.Write(&b, binary.BigEndian, t.CSVBlocks)
	_ = binary.Write(&b, binary.BigEndian, t.Until)
	if t.bonded() {
		b.Write(bondTermsTag)
		_ = binary.Write(&b, binary.BigEndian, t.BondAtoms)
		_ = binary.Write(&b, binary.BigEndian, t.BondLockBlocks)
	}
	return blake256.Sum256(b.Bytes()), nil
}

// bonded reports whether the table states bond terms at all.
func (t Terms) bonded() bool { return t.BondAtoms != 0 || t.BondLockBlocks != 0 }

// Join is one player's claim to a seat, signed by the key it announces.
//
// The signature is what lets a join be relayed. A peer that missed one learns
// it from anyone who did, and can check it rather than believe them - which is
// the difference between healing a lossy channel and letting any member inject
// whatever keys they like.
//
// It carries no identity. The sender's Bison Relay uid is the authenticated
// identity, and a relayed join arrives from a third party, so a uid inside it
// would be an unverifiable claim.
type Join struct {
	Key []byte // compressed session pubkey
	// LogKey is the per-match key this player's log entries are signed
	// with, and it is not the session key.
	//
	// The distinction is the whole reason it is announced here. A log key
	// is expected to become public the moment its owner misbehaves - its
	// signatures take their nonce from the position in the log, so
	// equivocating publishes it - and the session key holds the stake. One
	// key doing both would mean a cheat hands out the key to their own
	// escrow rather than forfeiting a bond.
	//
	// It needs no separate binding signature. The join is signed by the
	// session key over a digest that covers this, so a valid join *is* the
	// statement "the key that holds my stake adopts this log key for this
	// table" - and because the terms are in the digest too, it cannot be
	// lifted to a table where the log key would be expected fresh.
	LogKey []byte
	Bond   Bond
	Sig    []byte
}

// Bond is the deposit that makes a seat cost something.
//
// Without one a join is free, and free joins make every rule above them
// decorative: a stranger can fill a table's seats, or answer it once too often
// so nobody can form it, at no price and as many times as they like. zkidentity
// keys are free to generate, so the cost has to be locked coin rather than
// anything about who is asking.
//
// The script travels rather than just the outpoint, because it is what says the
// coin is locked at all - and PoP is what says this player controls it. A
// deposit anyone can name proves nothing on its own.
type Bond struct {
	Outpoint string
	Script   []byte
	// PoP is a signature by the bond's owner key over this join's session
	// key. It binds the two, so a bond cited in public cannot be lifted
	// into somebody else's join.
	PoP []byte
}

// Credentials are what a player sits down with: the key it plays under, and the
// bond that makes the seat cost something.
//
// They travel together because neither is any use alone. A session key with no
// bond is a free seat, and a bond nobody can tie to a session key is somebody
// else's deposit being cited.
type Credentials struct {
	// Session is the key this player's commitments are signed with, and the
	// key the escrow scripts name. It holds the stake.
	Session *secp256k1.PrivateKey
	// Log is the per-match key this player's log entries are signed with.
	// Separate from Session on purpose - see Join.LogKey - and never reused
	// between tables, because it is expected to become public if its owner
	// misbehaves.
	Log *secp256k1.PrivateKey
	// Bond is the key the deposit pays out to. It signs nothing but the
	// proof of possession.
	Bond         *secp256k1.PrivateKey
	BondOutpoint string
	BondScript   []byte
}

// Valid reports whether these could join anything.
func (c Credentials) Valid() error {
	switch {
	case c.Session == nil:
		return fmt.Errorf("no session key")
	case c.Log == nil:
		return fmt.Errorf("no log key")
	case c.Log.PubKey().IsEqual(c.Session.PubKey()):
		return fmt.Errorf("the log key is the session key, which would put the stake at risk of forfeiture")
	case c.Bond == nil:
		return fmt.Errorf("no bond key")
	case strings.TrimSpace(c.BondOutpoint) == "":
		return fmt.Errorf("no bond deposit")
	case len(c.BondScript) == 0:
		return fmt.Errorf("no bond script")
	}
	return nil
}

// SignJoin makes this player's claim to a seat at the table these terms
// describe.
//
// The proof of possession names the session key, so what is published cannot be
// lifted and presented by anybody else.
func SignJoin(t Terms, c Credentials) (*Join, error) {
	if err := c.Valid(); err != nil {
		return nil, err
	}
	priv, bondOutpoint, bondScript := c.Session, c.BondOutpoint, c.BondScript
	key := priv.PubKey().SerializeCompressed()
	logKey := c.Log.PubKey().SerializeCompressed()
	pop, err := escrow.SignBondPoP(bondOutpoint, key, c.Bond)
	if err != nil {
		return nil, err
	}

	j := &Join{
		Key:    key,
		LogKey: logKey,
		Bond:   Bond{Outpoint: bondOutpoint, Script: bondScript, PoP: pop},
	}
	digest, err := j.digest(t)
	if err != nil {
		return nil, err
	}
	sig, err := schnorr.Sign(priv, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign join: %w", err)
	}
	j.Sig = sig.Serialize()
	return j, nil
}

func (j *Join) digest(t Terms) ([32]byte, error) {
	th, err := t.Hash()
	if err != nil {
		return [32]byte{}, err
	}
	if len(j.Key) != escrow.PubKeyLen {
		return [32]byte{}, fmt.Errorf("join key is %d bytes, want %d", len(j.Key), escrow.PubKeyLen)
	}
	if len(j.LogKey) != escrow.PubKeyLen {
		return [32]byte{}, fmt.Errorf("join log key is %d bytes, want %d", len(j.LogKey), escrow.PubKeyLen)
	}
	var b bytes.Buffer
	b.Write(joinTag)
	b.Write(th[:])
	b.Write(j.Key)
	// Signing over the log key is what binds it to the session key, so no
	// separate binding message is needed and none can go missing.
	b.Write(j.LogKey)
	// The bond is signed over as well, so a relayed join cannot have its
	// deposit swapped for another on the way.
	writeString(&b, j.Bond.Outpoint)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(j.Bond.Script)))
	b.Write(j.Bond.Script)
	return blake256.Sum256(b.Bytes()), nil
}

// Verify checks that the key really claimed a seat at this table. Binding the
// terms is what stops a join collected for one table being relayed into
// another, which would enrol a key in a roster it never agreed to.
func (j *Join) Verify(t Terms) error {
	digest, err := j.digest(t)
	if err != nil {
		return err
	}
	if err := verifySig(j.Key, j.Sig, digest); err != nil {
		return err
	}
	// A join whose log key is its session key would have the player signing
	// the log with the key that holds their stake, so equivocating would
	// publish the escrow rather than forfeiting the bond.
	if bytes.Equal(j.Key, j.LogKey) {
		return fmt.Errorf("join names its session key as its log key, " +
			"which would put the stake at risk of forfeiture")
	}
	if _, err := secp256k1.ParsePubKey(j.LogKey); err != nil {
		return fmt.Errorf("join log key: %w", err)
	}

	// The bond, as far as it can be checked without a chain. Whether the
	// deposit exists is somebody else's question - see CheckBond - but
	// whether it is a bond at all, on terms worth anything, and whether
	// this player controls it, are all answerable here.
	terms, err := escrow.ParseBond(j.Bond.Script)
	if err != nil {
		return fmt.Errorf("bond script: %w", err)
	}
	if terms.LockBlocks < escrow.MinBondBlocks {
		return fmt.Errorf("bond is locked for %d blocks, under the %d minimum",
			terms.LockBlocks, escrow.MinBondBlocks)
	}
	if err := escrow.VerifyBondPoP(j.Bond.Script, j.Bond.Outpoint, j.Key, j.Bond.PoP); err != nil {
		return fmt.Errorf("bond: %w", err)
	}
	return nil
}

// BondFacts is what a chain says about a bond's deposit. A peer gets these from
// its own node - or, in a sandbox, from the host's - and hands them here.
type BondFacts struct {
	Found         bool
	ValueAtoms    int64
	PkScriptHex   string
	Confirmations int64
}

// CheckBond decides whether a join's bond is real, given what the chain says.
//
// It is separate from Verify because it needs facts nobody can sign for. Keeping
// the rule here rather than in whatever fetched them means every peer applies
// the same one, and that it can be checked without a chain at all.
func CheckBond(j *Join, facts BondFacts, params stdaddr.AddressParams) error {
	if j == nil {
		return fmt.Errorf("no join")
	}
	if !facts.Found {
		// Never existed, already spent, or not yet confirmed. For a
		// deposit that is supposed to be locked coin everyone can see,
		// those are the same answer.
		return fmt.Errorf("bond deposit %s is not unspent coin on chain", j.Bond.Outpoint)
	}

	_, pkScript, err := escrow.BondAddress(j.Bond.Script, params)
	if err != nil {
		return fmt.Errorf("bond address: %w", err)
	}
	if !strings.EqualFold(facts.PkScriptHex, hex.EncodeToString(pkScript)) {
		// The deposit pays something else. Citing an outpoint that
		// happens to hold coin is not the same as having locked any.
		return fmt.Errorf("bond deposit %s does not pay the bond script", j.Bond.Outpoint)
	}
	if facts.ValueAtoms < int64(escrow.MinBondAtoms) {
		return fmt.Errorf("bond holds %d atoms, under the %d minimum",
			facts.ValueAtoms, escrow.MinBondAtoms)
	}
	if facts.Confirmations < int64(escrow.BondConfirmations) {
		return fmt.Errorf("bond has %d confirmations, needs %d",
			facts.Confirmations, escrow.BondConfirmations)
	}
	return nil
}

// Assertion is a peer saying which membership it currently holds.
//
// It is signed, and that is not decoration. Everyone asserting the same thing
// is what lets a table form before its deadline, so an unsigned one would let
// anybody manufacture agreement - telling each peer that everyone concurs with
// whatever that peer happens to hold, and so driving peers with different join
// sets to bind different memberships.
//
// It is still revisable, which is the whole difference from a commit: a peer
// that learns another join says so and moves on. Nothing irreversible rests on
// one, so two conflicting assertions are not evidence of anything.
type Assertion struct {
	Roster [32]byte
	Signer []byte
	Sig    []byte
}

// SignAssertion says which membership this key currently holds.
func SignAssertion(t Terms, roster [32]byte, priv *secp256k1.PrivateKey) (*Assertion, error) {
	if priv == nil {
		return nil, fmt.Errorf("no signing key")
	}
	a := &Assertion{Roster: roster, Signer: priv.PubKey().SerializeCompressed()}
	digest, err := a.digest(t)
	if err != nil {
		return nil, err
	}
	sig, err := schnorr.Sign(priv, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign assertion: %w", err)
	}
	a.Sig = sig.Serialize()
	return a, nil
}

func (a *Assertion) digest(t Terms) ([32]byte, error) {
	th, err := t.Hash()
	if err != nil {
		return [32]byte{}, err
	}
	if len(a.Signer) != escrow.PubKeyLen {
		return [32]byte{}, fmt.Errorf("assertion signer is %d bytes, want %d", len(a.Signer), escrow.PubKeyLen)
	}
	var b bytes.Buffer
	b.Write(assertTag)
	b.Write(th[:])
	b.Write(a.Roster[:])
	return blake256.Sum256(b.Bytes()), nil
}

// Verify checks the assertion's signature against the key it names.
func (a *Assertion) Verify(t Terms) error {
	digest, err := a.digest(t)
	if err != nil {
		return err
	}
	return verifySig(a.Signer, a.Sig, digest)
}

// Commit binds its signer to one roster, for one session, irrevocably.
//
// This is the message settlement rests on, and it is separate from the roster
// assertions that heal the channel for exactly that reason: an assertion is a
// claim its sender may still revise, and a peer that settled on one could find
// its counterparty had moved on. A commit cannot be revised, so two commits
// from one key at one session are a self-contained proof rather than a
// complaint - the same shape gamelog uses for conflicting chain heads.
type Commit struct {
	Roster [32]byte
	Signer []byte
	Sig    []byte
}

// SignCommit binds a key to a roster.
func SignCommit(t Terms, roster [32]byte, priv *secp256k1.PrivateKey) (*Commit, error) {
	if priv == nil {
		return nil, fmt.Errorf("no signing key")
	}
	c := &Commit{Roster: roster, Signer: priv.PubKey().SerializeCompressed()}
	digest, err := c.digest(t)
	if err != nil {
		return nil, err
	}
	sig, err := schnorr.Sign(priv, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign commit: %w", err)
	}
	c.Sig = sig.Serialize()
	return c, nil
}

func (c *Commit) digest(t Terms) ([32]byte, error) {
	th, err := t.Hash()
	if err != nil {
		return [32]byte{}, err
	}
	if len(c.Signer) != escrow.PubKeyLen {
		return [32]byte{}, fmt.Errorf("commit signer is %d bytes, want %d", len(c.Signer), escrow.PubKeyLen)
	}
	var b bytes.Buffer
	b.Write(commitTag)
	b.Write(th[:])
	b.Write(c.Roster[:])
	return blake256.Sum256(b.Bytes()), nil
}

// Verify checks the commit's signature against the key it names.
func (c *Commit) Verify(t Terms) error {
	digest, err := c.digest(t)
	if err != nil {
		return err
	}
	return verifySig(c.Signer, c.Sig, digest)
}

// Funding is one member saying which output holds its stake.
//
// What makes it true is the chain, not this: the deposit script for a seat is
// derived from a membership everyone already agreed, so anyone can look the
// outpoint up and check for themselves that it pays that script, for at least
// the buy-in, with enough confirmations behind it. A member who announces
// something else is refused by the chain rather than by anyone's judgement.
//
// It is signed anyway, for the one question the chain cannot answer. If a
// member pays the same deposit script twice there are two outputs that satisfy
// it equally, and only that member can say which one is the stake. Unsigned,
// anybody at the table could choose on their behalf.
type Funding struct {
	Seat     uint32
	Outpoint string
	Signer   []byte
	Sig      []byte
}

// SignFunding says where this member's stake is.
func SignFunding(t Terms, seat uint32, outpoint string, priv *secp256k1.PrivateKey) (*Funding, error) {
	if priv == nil {
		return nil, fmt.Errorf("no signing key")
	}
	f := &Funding{
		Seat:     seat,
		Outpoint: strings.TrimSpace(outpoint),
		Signer:   priv.PubKey().SerializeCompressed(),
	}
	digest, err := f.digest(t)
	if err != nil {
		return nil, err
	}
	sig, err := schnorr.Sign(priv, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign funding: %w", err)
	}
	f.Sig = sig.Serialize()
	return f, nil
}

func (f *Funding) digest(t Terms) ([32]byte, error) {
	th, err := t.Hash()
	if err != nil {
		return [32]byte{}, err
	}
	if len(f.Signer) != escrow.PubKeyLen {
		return [32]byte{}, fmt.Errorf("funding signer is %d bytes, want %d", len(f.Signer), escrow.PubKeyLen)
	}
	if strings.TrimSpace(f.Outpoint) == "" {
		return [32]byte{}, fmt.Errorf("funding names no outpoint")
	}
	var b bytes.Buffer
	b.Write(fundedTag)
	b.Write(th[:])
	_ = binary.Write(&b, binary.BigEndian, f.Seat)
	writeString(&b, f.Outpoint)
	return blake256.Sum256(b.Bytes()), nil
}

// Verify checks the announcement's signature against the key it names.
//
// It says nothing about whether the outpoint exists, pays enough, or pays the
// right script at all. Those are the chain's answers and the caller has to go
// and get them; this establishes only who said it.
func (f *Funding) Verify(t Terms) error {
	digest, err := f.digest(t)
	if err != nil {
		return err
	}
	return verifySig(f.Signer, f.Sig, digest)
}

// Bonded is one member pointing at the output holding its forfeitable bond.
//
// Separate from Funding and for a different thing. A stake is what a player can
// lose at cards; a bond is what they lose for not playing - walking out of a
// hand, or holding the table up until everybody else gives up. A table deals
// only when both are on the chain for every seat, because a table without bonds
// has no answer to somebody who stops.
type Bonded struct {
	Seat     uint32
	Outpoint string
	Signer   []byte
	Sig      []byte
}

// SignBonded says where this member's forfeitable bond is.
func SignBonded(t Terms, seat uint32, outpoint string, priv *secp256k1.PrivateKey) (*Bonded, error) {
	if priv == nil {
		return nil, fmt.Errorf("no signing key")
	}
	b := &Bonded{
		Seat:     seat,
		Outpoint: strings.TrimSpace(outpoint),
		Signer:   priv.PubKey().SerializeCompressed(),
	}
	digest, err := b.digest(t)
	if err != nil {
		return nil, err
	}
	sig, err := schnorr.Sign(priv, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign bonded: %w", err)
	}
	b.Sig = sig.Serialize()
	return b, nil
}

func (b *Bonded) digest(t Terms) ([32]byte, error) {
	th, err := t.Hash()
	if err != nil {
		return [32]byte{}, err
	}
	if len(b.Signer) != escrow.PubKeyLen {
		return [32]byte{}, fmt.Errorf("bond signer is %d bytes, want %d", len(b.Signer), escrow.PubKeyLen)
	}
	if strings.TrimSpace(b.Outpoint) == "" {
		return [32]byte{}, fmt.Errorf("bond announcement names no outpoint")
	}
	var buf bytes.Buffer
	buf.Write(bondedTag)
	buf.Write(th[:])
	_ = binary.Write(&buf, binary.BigEndian, b.Seat)
	writeString(&buf, b.Outpoint)
	return blake256.Sum256(buf.Bytes()), nil
}

// Verify checks the announcement's signature against the key it names, and
// nothing else. Whether the output exists and pays the right script is the
// chain's answer, and the caller has to go and get it.
func (b *Bonded) Verify(t Terms) error {
	digest, err := b.digest(t)
	if err != nil {
		return err
	}
	return verifySig(b.Signer, b.Sig, digest)
}

// Payout is where a member wants coin sent that it did not pay for itself.
//
// A share of somebody's forfeited bond, mostly. It is signed because a claim is
// built by the others: without a signature a member could announce an address on
// a neighbour's behalf and quietly redirect their share of every forfeit at the
// table.
type Payout struct {
	Seat    uint32
	Address string
	Signer  []byte
	Sig     []byte
}

// SignPayout says where this member wants to be paid.
func SignPayout(t Terms, seat uint32, address string, priv *secp256k1.PrivateKey) (*Payout, error) {
	if priv == nil {
		return nil, fmt.Errorf("no signing key")
	}
	if strings.TrimSpace(address) == "" {
		return nil, fmt.Errorf("no payout address")
	}
	p := &Payout{
		Seat:    seat,
		Address: strings.TrimSpace(address),
		Signer:  priv.PubKey().SerializeCompressed(),
	}
	digest, err := p.digest(t)
	if err != nil {
		return nil, err
	}
	sig, err := schnorr.Sign(priv, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign payout: %w", err)
	}
	p.Sig = sig.Serialize()
	return p, nil
}

func (p *Payout) digest(t Terms) ([32]byte, error) {
	th, err := t.Hash()
	if err != nil {
		return [32]byte{}, err
	}
	if len(p.Signer) != escrow.PubKeyLen {
		return [32]byte{}, fmt.Errorf("payout signer is %d bytes, want %d", len(p.Signer), escrow.PubKeyLen)
	}
	if strings.TrimSpace(p.Address) == "" {
		return [32]byte{}, fmt.Errorf("payout names no address")
	}
	var b bytes.Buffer
	b.Write(payoutTag)
	b.Write(th[:])
	_ = binary.Write(&b, binary.BigEndian, p.Seat)
	writeString(&b, p.Address)
	return blake256.Sum256(b.Bytes()), nil
}

// Verify checks the announcement against the key it names. Whether the address
// is one this network understands is the caller's question, and it has to ask
// it: an address nobody can decode makes every claim built to pay it fail.
func (p *Payout) Verify(t Terms) error {
	digest, err := p.digest(t)
	if err != nil {
		return err
	}
	return verifySig(p.Signer, p.Sig, digest)
}

// ConflictingCommits is proof that one key bound itself to two rosters at one
// session. It is verifiable by anyone, offline, with no quorum and no vote.
type ConflictingCommits struct {
	A, B *Commit
}

// Verify checks that the pair really is a contradiction by its signer. Showing
// the same commit twice proves nothing, and neither does a pair whose
// signatures do not check.
func (p ConflictingCommits) Verify(t Terms) error {
	if p.A == nil || p.B == nil {
		return fmt.Errorf("a proof needs two commits")
	}
	if !bytes.Equal(p.A.Signer, p.B.Signer) {
		return fmt.Errorf("commits are by different keys, which is not a contradiction")
	}
	if p.A.Roster == p.B.Roster {
		return fmt.Errorf("both commits name the same roster")
	}
	if err := p.A.Verify(t); err != nil {
		return fmt.Errorf("first commit: %w", err)
	}
	if err := p.B.Verify(t); err != nil {
		return fmt.Errorf("second commit: %w", err)
	}
	return nil
}

// RosterHash is the digest a commit binds to: the terms, and the membership in
// the one canonical order every member derives independently.
func RosterHash(t Terms, canonical [][]byte) ([32]byte, error) {
	th, err := t.Hash()
	if err != nil {
		return [32]byte{}, err
	}
	var b bytes.Buffer
	b.Write(rosterTag)
	b.Write(th[:])
	_ = binary.Write(&b, binary.BigEndian, uint32(len(canonical)))
	for _, k := range canonical {
		if len(k) != escrow.PubKeyLen {
			return [32]byte{}, fmt.Errorf("member key is %d bytes, want %d", len(k), escrow.PubKeyLen)
		}
		b.Write(k)
	}
	return blake256.Sum256(b.Bytes()), nil
}

func writeString(b *bytes.Buffer, s string) {
	_ = binary.Write(b, binary.BigEndian, uint32(len(s)))
	b.WriteString(s)
}

func verifySig(key, sig []byte, digest [32]byte) error {
	if len(sig) != SigLen {
		return fmt.Errorf("signature is %d bytes, want %d", len(sig), SigLen)
	}
	pub, err := secp256k1.ParsePubKey(key)
	if err != nil {
		return fmt.Errorf("key: %w", err)
	}
	parsed, err := schnorr.ParseSignature(sig)
	if err != nil {
		return fmt.Errorf("parse signature: %w", err)
	}
	if !parsed.Verify(digest[:], pub) {
		return fmt.Errorf("signature does not verify")
	}
	return nil
}

// sortKeys orders compressed keys the way escrow.CanonicalMembers does, without
// its member-count limit. Selection has to be able to look at more keys than a
// table can seat in order to notice that it is oversubscribed.
func sortKeys(keys [][]byte) [][]byte {
	out := make([][]byte, len(keys))
	copy(out, keys)
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i], out[j]) < 0 })
	return out
}

func keyID(k []byte) string { return hex.EncodeToString(k) }
