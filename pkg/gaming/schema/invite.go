package schema

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
)

// A game invite is an ordinary chat message, not a wire frame.
//
// Gameplay rides the hidden --gaming[...]-- envelope because it is between the
// running games and must never read as conversation. An invitation is the
// opposite in both respects. It is addressed to a person, so it has to stay
// legible to whoever receives it - including someone whose client knows nothing
// about games, who should see an invitation they can act on rather than
// silence. And it is what *starts* a game, so at the moment it arrives there is
// no game running to deliver it to.
//
//	gaming://poker/table?buyin=10000000&seats=6&sid=<hex>
//
// The host is the game id, the same routing key the wire envelope carries, so
// one renderer in a host serves every game. Only the terms a host can present
// honestly are standardised - what it costs and how many seats - and anything
// else is the game's own business.
const InviteScheme = "gaming"

// InviteKindTable offers a seat at a table.
const InviteKindTable = "table"

var (
	inviteGameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	inviteKindRE = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	// inviteSIDRE must stay the shape the wire accepts as a session id.
	inviteSIDRE = regexp.MustCompile(`^[0-9a-f]{1,32}$`)
)

// Invite is an offer to join a game.
type Invite struct {
	Game string
	Kind string
	// BuyInAtoms is the stake a seat costs. Zero states no terms.
	BuyInAtoms uint64
	// Seats is how many the table holds. Zero states none.
	Seats uint32
	// SID identifies the session the invite refers to.
	SID string
	// CSVBlocks is the relative timelock every seat's refund branch
	// carries. It is stated in the invitation because each member builds
	// their own branch and everyone has to be able to check everyone
	// else's: a member whose refund matured in a block could pull their
	// stake mid-hand. Zero states no terms.
	CSVBlocks uint32
	// Until is the block height after which no more joins are admitted. It
	// is a height rather than a time so every peer reads the same deadline;
	// clocks disagree, and nobody can be shown to have read one wrong. Zero
	// states no terms.
	Until uint32
}

// String renders the invite as the link that goes in a chat message.
func (i Invite) String() (string, error) {
	game := strings.ToLower(strings.TrimSpace(i.Game))
	kind := strings.ToLower(strings.TrimSpace(i.Kind))
	if !inviteGameRE.MatchString(game) {
		return "", fmt.Errorf("invite game %q is not a routing key", i.Game)
	}
	if !inviteKindRE.MatchString(kind) {
		return "", fmt.Errorf("invite kind %q is not valid", i.Kind)
	}

	q := url.Values{}
	if i.BuyInAtoms > 0 {
		q.Set("buyin", strconv.FormatUint(i.BuyInAtoms, 10))
	}
	if i.Seats > 0 {
		q.Set("seats", strconv.FormatUint(uint64(i.Seats), 10))
	}
	if i.SID != "" {
		q.Set("sid", i.SID)
	}
	if i.CSVBlocks > 0 {
		q.Set("csv", strconv.FormatUint(uint64(i.CSVBlocks), 10))
	}
	if i.Until > 0 {
		q.Set("until", strconv.FormatUint(uint64(i.Until), 10))
	}

	out := fmt.Sprintf("%s://%s/%s", InviteScheme, game, kind)
	if encoded := q.Encode(); encoded != "" {
		out += "?" + encoded
	}
	return out, nil
}

// ParseInvite reads an invite link back.
//
// It exists so the format has one definition with a round trip that can be
// tested, rather than a writer here and a reader in a host that drift until
// somebody's invitation stops rendering.
func ParseInvite(link string) (Invite, error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return Invite{}, fmt.Errorf("parse invite: %w", err)
	}
	if u.Scheme != InviteScheme {
		return Invite{}, fmt.Errorf("not a %s:// link", InviteScheme)
	}

	game := strings.ToLower(u.Host)
	kind := strings.ToLower(strings.Trim(u.Path, "/"))
	if !inviteGameRE.MatchString(game) {
		return Invite{}, fmt.Errorf("invite names no game")
	}
	if !inviteKindRE.MatchString(kind) {
		return Invite{}, fmt.Errorf("invite names no kind")
	}

	inv := Invite{Game: game, Kind: kind, SID: u.Query().Get("sid")}
	// The session id becomes the frame's routing key, which is 1-32
	// lowercase hex. Refusing it here means an unusable invitation is
	// rejected when it is read, rather than accepted and then failing to
	// encode at the moment somebody tries to send their first frame.
	if inv.SID != "" && !inviteSIDRE.MatchString(inv.SID) {
		return Invite{}, fmt.Errorf("invite session %q is not 1-32 lowercase hex", inv.SID)
	}
	if v := u.Query().Get("buyin"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return Invite{}, fmt.Errorf("invite buy-in %q is not a number of atoms", v)
		}
		inv.BuyInAtoms = n
	}
	if v := u.Query().Get("seats"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return Invite{}, fmt.Errorf("invite seats %q is not a number", v)
		}
		// A table nobody could sit at, or more seats than an escrow
		// script can name, is not an invitation to anything.
		if n < 2 || n > uint64(escrow.MaxMembers) {
			return Invite{}, fmt.Errorf("invite seats %d is outside 2..%d", n, escrow.MaxMembers)
		}
		inv.Seats = uint32(n)
	}
	if v := u.Query().Get("csv"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil || n == 0 {
			return Invite{}, fmt.Errorf("invite refund timelock %q is not a number of blocks", v)
		}
		inv.CSVBlocks = uint32(n)
	}
	if v := u.Query().Get("until"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil || n == 0 {
			return Invite{}, fmt.Errorf("invite deadline %q is not a block height", v)
		}
		inv.Until = uint32(n)
	}
	return inv, nil
}
