package schema

import (
	"encoding/hex"
	"fmt"

	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

// The wire shapes above are hex strings; the formation logic works in bytes and
// knows nothing about this package. These convert between them, in one place,
// so neither side has to know how the other spells a key.
//
// Conversion validates nothing beyond decoding. Whether a join is genuine is
// membership's answer, and asking it twice in two places is how the two answers
// eventually differ.

// TermsFrom renders terms for the wire.
func TermsFrom(t membership.Terms) Terms {
	return Terms{
		Game:       t.Game,
		GameVer:    t.GameVer,
		SID:        t.SID,
		BuyInAtoms: t.BuyInAtoms,
		Seats:      t.Seats,
		CSVBlocks:  t.CSVBlocks,
		Until:      t.Until,
		BondAtoms:  t.BondAtoms, BondLockBlocks: t.BondLockBlocks,
	}
}

// Into reads terms back.
func (t Terms) Into() membership.Terms {
	return membership.Terms{
		Game:       t.Game,
		GameVer:    t.GameVer,
		SID:        t.SID,
		BuyInAtoms: t.BuyInAtoms,
		Seats:      t.Seats,
		CSVBlocks:  t.CSVBlocks,
		Until:      t.Until,
		BondAtoms:  t.BondAtoms, BondLockBlocks: t.BondLockBlocks,
	}
}

// JoinFrom renders a join for the wire.
func JoinFrom(j *membership.Join) Join {
	if j == nil {
		return Join{}
	}
	return Join{
		Key:          hex.EncodeToString(j.Key),
		LogKey:       hex.EncodeToString(j.LogKey),
		Sig:          hex.EncodeToString(j.Sig),
		BondOutpoint: j.Bond.Outpoint,
		BondScript:   hex.EncodeToString(j.Bond.Script),
		BondPoP:      hex.EncodeToString(j.Bond.PoP),
	}
}

// Into reads a join back. The signature it carries is checked by membership
// against the terms the reader holds, not here.
func (j Join) Into() (*membership.Join, error) {
	key, err := hex.DecodeString(j.Key)
	if err != nil {
		return nil, fmt.Errorf("join key: %w", err)
	}
	logKey, err := hex.DecodeString(j.LogKey)
	if err != nil {
		return nil, fmt.Errorf("join log key: %w", err)
	}
	sig, err := hex.DecodeString(j.Sig)
	if err != nil {
		return nil, fmt.Errorf("join signature: %w", err)
	}
	script, err := hex.DecodeString(j.BondScript)
	if err != nil {
		return nil, fmt.Errorf("join bond script: %w", err)
	}
	pop, err := hex.DecodeString(j.BondPoP)
	if err != nil {
		return nil, fmt.Errorf("join bond proof: %w", err)
	}
	return &membership.Join{
		Key:    key,
		LogKey: logKey,
		Sig:    sig,
		Bond: membership.Bond{
			Outpoint: j.BondOutpoint,
			Script:   script,
			PoP:      pop,
		},
	}, nil
}

// CommitFrom renders a commit for the wire.
func CommitFrom(c *membership.Commit) Commit {
	if c == nil {
		return Commit{}
	}
	return Commit{
		Roster: hex.EncodeToString(c.Roster[:]),
		Signer: hex.EncodeToString(c.Signer),
		Sig:    hex.EncodeToString(c.Sig),
	}
}

// Into reads a commit back.
func (c Commit) Into() (*membership.Commit, error) {
	roster, err := hex.DecodeString(c.Roster)
	if err != nil {
		return nil, fmt.Errorf("commit roster: %w", err)
	}
	if len(roster) != 32 {
		return nil, fmt.Errorf("commit roster is %d bytes, want 32", len(roster))
	}
	signer, err := hex.DecodeString(c.Signer)
	if err != nil {
		return nil, fmt.Errorf("commit signer: %w", err)
	}
	sig, err := hex.DecodeString(c.Sig)
	if err != nil {
		return nil, fmt.Errorf("commit signature: %w", err)
	}
	out := &membership.Commit{Signer: signer, Sig: sig}
	copy(out.Roster[:], roster)
	return out, nil
}

// FundedFrom renders a funding announcement for the wire.
func FundedFrom(f *membership.Funding) Funded {
	if f == nil {
		return Funded{}
	}
	return Funded{
		Seat:     f.Seat,
		Outpoint: f.Outpoint,
		Signer:   hex.EncodeToString(f.Signer),
		Sig:      hex.EncodeToString(f.Sig),
	}
}

// Into reads a funding announcement back.
func (f Funded) Into() (*membership.Funding, error) {
	signer, err := hex.DecodeString(f.Signer)
	if err != nil {
		return nil, fmt.Errorf("funded signer: %w", err)
	}
	sig, err := hex.DecodeString(f.Sig)
	if err != nil {
		return nil, fmt.Errorf("funded signature: %w", err)
	}
	return &membership.Funding{
		Seat:     f.Seat,
		Outpoint: f.Outpoint,
		Signer:   signer,
		Sig:      sig,
	}, nil
}

// RosterFrom renders the membership a peer holds, with the joins it was
// computed from, so a peer that missed one can check its way to the same
// answer instead of believing this one.
func RosterFrom(terms membership.Terms, seats map[uint32][]byte, joins []*membership.Join, assertion *membership.Assertion) Roster {
	out := Roster{
		Seats: make(map[uint32]string, len(seats)),
		Joins: make([]Join, 0, len(joins)),
	}
	for seat, key := range seats {
		out.Seats[seat] = hex.EncodeToString(key)
	}
	for _, j := range joins {
		out.Joins = append(out.Joins, JoinFrom(j))
	}
	t := TermsFrom(terms)
	out.Terms = &t
	if assertion != nil {
		out.Roster = hex.EncodeToString(assertion.Roster[:])
		out.Signer = hex.EncodeToString(assertion.Signer)
		out.Sig = hex.EncodeToString(assertion.Sig)
	}
	return out
}

// Assertion reads back the signed claim a roster message carries, if it carries
// one. A message with no claim is just a carrier for joins.
func (r Roster) Assertion() (*membership.Assertion, error) {
	if r.Roster == "" && r.Signer == "" && r.Sig == "" {
		return nil, nil
	}
	roster, err := hex.DecodeString(r.Roster)
	if err != nil {
		return nil, fmt.Errorf("assertion roster: %w", err)
	}
	if len(roster) != 32 {
		return nil, fmt.Errorf("assertion roster is %d bytes, want 32", len(roster))
	}
	signer, err := hex.DecodeString(r.Signer)
	if err != nil {
		return nil, fmt.Errorf("assertion signer: %w", err)
	}
	sig, err := hex.DecodeString(r.Sig)
	if err != nil {
		return nil, fmt.Errorf("assertion signature: %w", err)
	}
	out := &membership.Assertion{Signer: signer, Sig: sig}
	copy(out.Roster[:], roster)
	return out, nil
}

// SeatKeys reads a roster's seats back as bytes, which is the shape the action
// log and the escrow scripts both want.
func (r Roster) SeatKeys() (map[uint32][]byte, error) {
	out := make(map[uint32][]byte, len(r.Seats))
	for seat, h := range r.Seats {
		key, err := hex.DecodeString(h)
		if err != nil {
			return nil, fmt.Errorf("seat %d key: %w", seat, err)
		}
		out[seat] = key
	}
	return out, nil
}

// Bonded is one member pointing at the output holding its forfeitable bond.
type Bonded struct {
	Seat     uint32 `json:"seat"`
	Outpoint string `json:"outpoint"`
	Signer   string `json:"signer"` // hex compressed session pubkey
	Sig      string `json:"sig"`
}

// BondedFrom renders a bond announcement for the wire.
func BondedFrom(b *membership.Bonded) Bonded {
	if b == nil {
		return Bonded{}
	}
	return Bonded{
		Seat:     b.Seat,
		Outpoint: b.Outpoint,
		Signer:   hex.EncodeToString(b.Signer),
		Sig:      hex.EncodeToString(b.Sig),
	}
}

// Into reads a bond announcement back. Its signature is checked by membership
// against the terms the reader holds, and the output itself against the chain.
func (b Bonded) Into() (*membership.Bonded, error) {
	signer, err := hex.DecodeString(b.Signer)
	if err != nil {
		return nil, fmt.Errorf("bond signer: %w", err)
	}
	sig, err := hex.DecodeString(b.Sig)
	if err != nil {
		return nil, fmt.Errorf("bond signature: %w", err)
	}
	return &membership.Bonded{
		Seat:     b.Seat,
		Outpoint: b.Outpoint,
		Signer:   signer,
		Sig:      sig,
	}, nil
}

// Payout is where a member wants coin sent that it did not pay for itself.
type Payout struct {
	Seat    uint32 `json:"seat"`
	Address string `json:"address"`
	Signer  string `json:"signer"` // hex compressed session pubkey
	Sig     string `json:"sig"`
}

// PayoutFrom renders a payout announcement for the wire.
func PayoutFrom(p *membership.Payout) Payout {
	if p == nil {
		return Payout{}
	}
	return Payout{
		Seat:    p.Seat,
		Address: p.Address,
		Signer:  hex.EncodeToString(p.Signer),
		Sig:     hex.EncodeToString(p.Sig),
	}
}

// Into reads a payout announcement back.
func (p Payout) Into() (*membership.Payout, error) {
	signer, err := hex.DecodeString(p.Signer)
	if err != nil {
		return nil, fmt.Errorf("payout signer: %w", err)
	}
	sig, err := hex.DecodeString(p.Sig)
	if err != nil {
		return nil, fmt.Errorf("payout signature: %w", err)
	}
	return &membership.Payout{
		Seat:    p.Seat,
		Address: p.Address,
		Signer:  signer,
		Sig:     sig,
	}, nil
}
