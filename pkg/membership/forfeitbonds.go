package membership

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
)

// ForfeitableBond is where one seat's equivocation bond must be paid.
//
// Its own type rather than TableBond, so the two bond kinds cannot be handed
// to a call site meant for the other: a table bond answers silence and a
// forfeitable bond answers lying, and the places that must never confuse them
// are exactly the places a shared type would let them through.
type ForfeitableBond struct {
	Seat        uint32
	ScriptHex   string
	PkScriptHex string
	Address     string
}

// ForfeitableBonds derives every seat's forfeitable bond from the settled
// roster, the log-key roster, and the announced punishment keys.
//
// Deriving them here rather than letting each seat state its own is what makes
// the branch set checkable - the precondition escrow.ForfeitableBondScript
// documents for itself. A peer rebuilds its neighbour's script from what was
// already agreed and refuses anything that does not match byte for byte, so an
// extra branch keyed to a point the owner alone holds has nowhere to hide.
// Branches sit in the canonical member order, the same discipline every other
// script here uses, so the same roster always yields the same bytes.
//
// A seat whose announced punishment key is an opponent's log key, or its
// negation, gets no branch in that opponent's bond - forfeit.ForfeitKey names
// the reason - and the bond is built from the remaining seats. A bond left
// with no branches at all is refused.
func ForfeitableBonds(match string, seats, logs, punish map[uint32][]byte,
	lockBlocks uint32, params stdaddr.AddressParams) ([]ForfeitableBond, error) {

	if len(seats) < 2 {
		return nil, fmt.Errorf("a table of %d seats has no bonds to derive", len(seats))
	}
	members := make([][]byte, 0, len(seats))
	logPubs := make(map[uint32]*secp256k1.PublicKey, len(seats))
	for seat, key := range seats {
		lb, ok := logs[seat]
		if !ok {
			return nil, fmt.Errorf("seat %d has no log key", seat)
		}
		lp, err := secp256k1.ParsePubKey(lb)
		if err != nil {
			return nil, fmt.Errorf("seat %d log key: %w", seat, err)
		}
		pb, ok := punish[seat]
		if !ok {
			return nil, fmt.Errorf("seat %d has no punishment key", seat)
		}
		if _, err := secp256k1.ParsePubKey(pb); err != nil {
			return nil, fmt.Errorf("seat %d punishment key: %w", seat, err)
		}
		logPubs[seat] = lp
		members = append(members, key)
	}
	if len(logs) != len(seats) || len(punish) != len(seats) {
		return nil, fmt.Errorf("the rosters disagree: %d seats, %d log keys, %d punishment keys",
			len(seats), len(logs), len(punish))
	}
	canonical, err := escrow.CanonicalMembers(members)
	if err != nil {
		return nil, err
	}
	seatOf := make(map[string]uint32, len(seats))
	for seat, key := range seats {
		seatOf[string(key)] = seat
	}

	out := make([]ForfeitableBond, 0, len(seats))
	for seat, owner := range seats {
		branches := make([][]byte, 0, len(seats)-1)
		for _, member := range canonical {
			opp := seatOf[string(member)]
			if opp == seat {
				continue
			}
			// The two announcements ForfeitKey refuses; that opponent gets
			// no branch here rather than stopping the whole table.
			lb, pb := logs[seat], punish[opp]
			if bytes.Equal(lb, pb) || (lb[0] != pb[0] && bytes.Equal(lb[1:], pb[1:])) {
				continue
			}
			punishPub, err := secp256k1.ParsePubKey(pb)
			if err != nil {
				return nil, fmt.Errorf("seat %d punishment key: %w", opp, err)
			}
			fkey, err := forfeit.ForfeitKey(forfeit.Branch{Match: match, Seat: member},
				logPubs[seat], punishPub)
			if err != nil {
				return nil, fmt.Errorf("seat %d bond, branch for seat %d: %w", seat, opp, err)
			}
			branches = append(branches, fkey.SerializeCompressed())
		}
		script, err := escrow.ForfeitableBondScript(owner, branches, lockBlocks)
		if err != nil {
			return nil, fmt.Errorf("seat %d bond: %w", seat, err)
		}
		pkHex, addr, err := PkScriptAndAddr(script, params)
		if err != nil {
			return nil, fmt.Errorf("seat %d bond: %w", seat, err)
		}
		out = append(out, ForfeitableBond{
			Seat:        seat,
			ScriptHex:   hex.EncodeToString(script),
			PkScriptHex: pkHex,
			Address:     addr,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seat < out[j].Seat })
	return out, nil
}
