// Package membership holds who is in one match: which session key sits in
// which seat, and the deposit scripts that membership commits to.
//
// It lives outside the server because the server is on its way out. A table
// formed peer to peer has no referee to ask, so every player has to derive the
// same seats and the same deposit scripts from the same keys - and the moment
// two implementations of that exist, they can disagree by a byte and send money
// somewhere nobody can spend it. This is the same reasoning that put the script
// construction in pkg/escrow, applied one layer up: the membership a script
// commits to is as load-bearing as the script.
//
// Nothing here knows about poker. Seats, stakes and rosters are the lobby, and
// the lobby is game-agnostic; cards are somebody else's problem.
package membership

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
)

// Roster is the set of session keys the escrow scripts of one match commit to.
//
// Every member's settlement branch names every member, so a key arriving late
// changes the script - and so the deposit address - of everyone who has already
// funded. Funding therefore cannot open until the last seat has published, and
// the roster is frozen the moment it does.
type Roster struct {
	Size    int               // seats that must publish before funding opens
	Seats   map[uint32][]byte // seat -> compressed session key
	Escrows map[uint32]string // seat -> the id of the stake that seat posted
	// Members is the canonical roster the scripts were built from. Non-nil
	// only once the roster closed, which is what makes it frozen: a later
	// arrival can no longer change a key or take over a seat.
	Members [][]byte
}

// NewRoster returns an open roster for a table of the given size.
func NewRoster(size int) *Roster {
	return &Roster{
		Size:    size,
		Seats:   make(map[uint32][]byte),
		Escrows: make(map[uint32]string),
	}
}

// Closed reports whether the roster is frozen and its scripts derivable.
func (r *Roster) Closed() bool { return r != nil && r.Members != nil }

// Pending reports how many seats have still to publish a key.
func (r *Roster) Pending() uint32 {
	if r == nil || r.Size <= len(r.Seats) {
		return 0
	}
	return uint32(r.Size - len(r.Seats))
}

// Member is one seat's contribution to the roster: the key it plays under and
// the timelock its own refund branch carries.
//
// The CSV is per member rather than per table because the refund branch is
// unilateral - it is that member's own escape hatch - so it is theirs to state
// and everyone else's to check.
type Member struct {
	Seat       uint32
	CompPubkey []byte
	CSVBlocks  uint32
}

// Deposit is where one seat's stake must be paid.
type Deposit struct {
	Seat            uint32
	RedeemScriptHex string
	PkScriptHex     string
	DepositAddr     string
}

// Close derives every member's deposit script from the whole membership.
//
// Each member gets a script of their own: the settlement branch names the whole
// table, so no member can move their stake without every other member's
// signature, and the refund branch names only them, so recovering their own
// funds after the timelock never depends on anyone else being online.
//
// It computes and returns rather than writing through to whatever holds the
// members. That is deliberate: the version this replaced wrote each script into
// its escrow as it went, so a failure part way left a roster still open with
// some seats already carrying scripts derived from a membership that was never
// agreed. Here nothing is written unless everything derived.
func Close(members []Member, params stdaddr.AddressParams) (canonical [][]byte, deposits []Deposit, err error) {
	keys := make([][]byte, 0, len(members))
	for _, m := range members {
		keys = append(keys, m.CompPubkey)
	}
	canonical, err = escrow.CanonicalMembers(keys)
	if err != nil {
		return nil, nil, err
	}

	deposits = make([]Deposit, 0, len(members))
	for _, m := range members {
		redeem, err := escrow.RedeemScript(m.CompPubkey, canonical, m.CSVBlocks)
		if err != nil {
			return nil, nil, fmt.Errorf("seat %d: %w", m.Seat, err)
		}
		pkHex, addr, err := PkScriptAndAddr(redeem, params)
		if err != nil {
			return nil, nil, fmt.Errorf("seat %d: %w", m.Seat, err)
		}
		deposits = append(deposits, Deposit{
			Seat:            m.Seat,
			RedeemScriptHex: hex.EncodeToString(redeem),
			PkScriptHex:     pkHex,
			DepositAddr:     addr,
		})
	}

	// Seat order, so the same membership always yields the same slice. The
	// caller may have built members by ranging a map, and a result whose
	// order depends on that is a difference waiting to be depended on.
	sort.Slice(deposits, func(i, j int) bool { return deposits[i].Seat < deposits[j].Seat })
	return canonical, deposits, nil
}

// PkScriptAndAddr builds the P2SH pkScript (hex) and the human-readable address
// a redeem script's deposits are paid to.
func PkScriptAndAddr(redeem []byte, params stdaddr.AddressParams) (pkHex, addr string, err error) {
	a, pkScript, err := escrow.Address(redeem, params)
	if err != nil {
		return "", "", err
	}
	return hex.EncodeToString(pkScript), a.String(), nil
}

// MatchID derives a match key from a table and an optional session. Winner-take
// all tables use the table id alone; a session id is appended only when the
// caller supplies one.
//
// Note for anything that puts this on the wire: the result is not necessarily a
// valid frame session id, which must be 1-32 lowercase hex. A table id alone
// is; a table id with a session appended is not.
func MatchID(tableID, sessionID string) string {
	tableID = strings.TrimSpace(tableID)
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return tableID
	}
	return tableID + "|" + sessionID
}

// TableBondBlocks is how long a table bond stays locked before its owner can
// take it back alone.
//
// The backstop, not the punishment. It has to outlast the table by enough that
// nobody can sit out their own claim window, and it has to be long enough to
// still be a cost - a bond reclaimable the same evening deters nothing. A week
// is what the standing registration bond already uses, so a player holds one
// cost rather than learning two.
const TableBondBlocks uint32 = escrow.MinBondBlocks

// TableBond is where one seat's forfeitable bond must be paid.
type TableBond struct {
	Seat        uint32
	ScriptHex   string
	PkScriptHex string
	Address     string
}

// TableBonds derives every seat's forfeitable bond from the settled roster.
//
// One per seat, and every one of them names the whole table: the branch that
// takes a bond needs every member except its owner, so a bond derived from
// anything less than the agreed membership is one the table could never claim.
// Deriving them here rather than letting each player state its own is what makes
// that checkable - a peer rebuilds its neighbour's script from the roster it
// already agreed and refuses anything that does not match.
//
// Distinct from Deposits, which is where the money at stake goes. A stake is
// what a player can lose at cards; this is what they lose for not playing.
func TableBonds(seats map[uint32][]byte, params stdaddr.AddressParams) ([]TableBond, error) {
	if len(seats) < 2 {
		return nil, fmt.Errorf("a table of %d seats has no bonds to derive", len(seats))
	}
	members := make([][]byte, 0, len(seats))
	for _, key := range seats {
		members = append(members, key)
	}
	canonical, err := escrow.CanonicalMembers(members)
	if err != nil {
		return nil, err
	}

	out := make([]TableBond, 0, len(seats))
	for seat, owner := range seats {
		script, err := escrow.TableBondScript(owner, canonical, TableBondBlocks)
		if err != nil {
			return nil, fmt.Errorf("seat %d bond: %w", seat, err)
		}
		pkHex, addr, err := PkScriptAndAddr(script, params)
		if err != nil {
			return nil, fmt.Errorf("seat %d bond: %w", seat, err)
		}
		out = append(out, TableBond{
			Seat:        seat,
			ScriptHex:   hex.EncodeToString(script),
			PkScriptHex: pkHex,
			Address:     addr,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seat < out[j].Seat })
	return out, nil
}
