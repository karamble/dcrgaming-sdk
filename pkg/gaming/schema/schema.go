// Package schema is what a table says to itself, independent of how it is
// carried.
//
// The messages here are the game's own vocabulary. They travel inside
// --gaming[…]-- frames over Bison Relay, and today the same information also
// moves over gRPC; neither transport is part of their definition. That is the
// point. The gRPC surface is transitional, so anything defined in terms of it
// would have to be defined again later, and a signature over a payload would
// have to be recomputed rather than carried.
//
// Two rules run through all of it.
//
// Identity is never a field. The sender's Bison Relay uid is the authenticated
// identity, and a payload claiming one could only ever be a way to lie about
// it. Where a message needs to say which seat acted, it carries a signature
// that proves it rather than a name that asserts it.
//
// Seat, action, amount, signature and chain head are payload, never envelope.
// If a router or a content filter ever needed to understand a fold in order to
// move it, the separation would already have failed.
package schema

import (
	"encoding/json"
	"fmt"

	"github.com/karamble/dcrgaming-sdk/pkg/gamelog"
)

// Version is the game protocol version, carried as gv in the envelope and
// separate from the framing version.
//
// The dealing frames - card key, shuffle, leaving - must carry the signature of
// the seat they name, and a peer refuses one without it. There is deliberately no
// mode that accepts unsigned frames, because such a mode is the hole itself, held
// open by a flag somebody forgets. Peers on different versions therefore do not
// play together, which is the intended behaviour rather than something to work
// around.
//
// 3 changes the table bond: a claim no longer takes it but moves it into a claimed
// bond, where its owner has a window to answer with its own key. The bond script
// loses its claim branch, so its address changes, and a bond posted under an
// earlier version is a bond under earlier rules.
//
// 4 adds audit on challenge: any seat may demand a settled hand be recomputed
// from every seat's deck secrets, and refusing to reveal is answered by the
// claim. A peer on 3 cannot be asked to reveal, so the two do not play together.
//
// 5 adds the card-key possession proof and the shuffle dispute. A card key now
// carries a Schnorr proof that its announcer knows its discrete log, folded
// into the signed digest - a key without one is refused, so 4 and 5 do not
// play together. And a signed shuffle that fails verification can be disputed
// with a complaint that carries everything a verdict needs; every peer judges
// it identically, the hand is void, and the table settles at the last signed
// boundary without unanimity.
const Version = 5

// Game is the routing key for poker traffic.
const Game = "poker"

// Kind identifies a message. It is a string so an unknown one reads as itself
// in a log rather than as a number nobody can place.
type Kind string

const (
	// KindJoin is one player claiming a seat, signed by the key it
	// announces. The signature is what lets it be relayed: a peer that
	// missed one learns it from anyone who did and checks it, rather than
	// taking their word for a key.
	KindJoin Kind = "join"

	// KindRoster announces the seats a table is playing with and the
	// session keys their actions will be signed by, along with the joins
	// they were computed from.
	//
	// It is revisable, and nobody acts on it. It exists to heal a channel
	// that loses messages, so a peer holding fewer joins than it should can
	// catch up. Settling on it would be settling on somebody's current
	// opinion - that is what KindCommit is for.
	KindRoster Kind = "roster"

	// KindCommit binds its signer to one membership, for one session,
	// irrevocably. Two of them from one key at one session are a proof
	// anybody can check, the way two chain heads at one sequence are.
	KindCommit Kind = "commit"

	// KindFunded is one member saying which output holds its stake. The
	// chain is what makes it true; this only says who to attribute it to.
	KindFunded Kind = "funded"

	// KindAction is one signed entry of the action log.
	KindAction Kind = "action"

	// KindHead is a seat's signature over what it believes the log head to
	// be. Under group-chat fan-out no member sees another's stream, so
	// these are how a fork is found at all.
	KindHead Kind = "head"

	// KindResync asks for the entries a member is missing, and answers.
	KindResync      Kind = "resync"
	KindResyncReply Kind = "resync_reply"

	// KindDispute carries evidence that a seat contradicted itself. It is
	// self-contained: whoever receives it can check it without asking
	// anyone, which is what keeps it different from an accusation.
	KindDispute Kind = "dispute"

	// KindCheckpoint is a seat's signature over the stacks at the end of a
	// hand.
	//
	// It is what a table settles on when it stops unexpectedly. A hand in
	// progress was never agreed by anybody - one seat is mid-decision and
	// the others have not seen it - so there is nothing to settle it by,
	// and the last checkpoint is the most recent thing everyone put their
	// name to.
	KindCheckpoint Kind = "checkpoint"

	// KindCardKey is one seat's deck key for one hand, and KindShuffle and
	// KindShare are the dealing itself.
	//
	// They are the bulk of what a table sends. A shuffle carries a whole
	// masked deck and its proof, once per seat per hand; a share is small
	// and there are many.
	KindCardKey Kind = "cardkey"
	KindShuffle Kind = "shuffle"
	KindShare   Kind = "share"

	// KindBonded is one member saying which output holds its forfeitable
	// bond. Like KindFunded, the chain is what makes it true.
	KindBonded Kind = "bonded"

	// KindAccusation is a signature on a future accusation against a seat,
	// agreed while the table is still cooperating.
	//
	// It has to be pre-signed, because an accusation needs every member
	// including the accused, who would not sign once accused. It pays into the
	// claimed bond, where its target answers with its own key alone - so what
	// is gathered here is the ability to accuse, and a peer that loses it has
	// lost no defence.
	KindAccusation Kind = "accusation"

	// KindSettle is a signature on the transaction that pays a table out.
	KindSettle Kind = "settle"

	// KindRelease is a signature on the transaction that hands one seat's
	// table bond back when the table has finished. The alive branch of the
	// bond, which every member signs and which carries no timelock - so a
	// table that ends properly returns its bonds at once rather than leaving
	// each player to wait out the week the backstop takes.
	KindRelease Kind = "release"

	// KindPayout is one member saying where it wants to be paid.
	KindPayout Kind = "payout"

	// KindLeaving is a seat saying it is getting up.
	//
	// Not the same as going quiet, which is the whole reason it exists: a
	// player who says so folds the hand they are in and the table settles at
	// its next boundary, where one who simply stops is answered by a claim
	// on their bond.
	KindLeaving Kind = "leaving"

	// KindClaim says an accusation has been broadcast, and carries it.
	//
	// Nothing is gathered by it and nobody weighs it: the accusation was
	// agreed before the dispute and the chain is what decides. It is said out
	// loud only so a peer that is listening learns it is accused without
	// waiting for a confirmation - and the accused watches the chain too,
	// precisely because this message can be withheld.
	KindClaim Kind = "claim"

	// KindTake proposes taking a claimed bond whose window has closed, for the
	// other seats to co-sign.
	//
	// The forfeiture, and the only part of a dispute that is agreed at the
	// time: the accusation was pre-signed and the answer needs one key, but
	// this needs the seats it pays and they are willing when it happens.
	KindTake Kind = "take"

	// KindChallenge is a seat demanding a settled hand be recomputed from
	// everything everybody knew. It obliges every seat to reveal that hand's
	// secrets, and refusing is what the claim answers.
	KindChallenge Kind = "challenge"
	// KindSecrets is one seat giving a challenged hand's secrets away -
	// worthless to anybody but an auditor, which is the point.
	KindSecrets Kind = "secrets"

	// KindShuffleComplaint is a seat disputing a shuffle it verified and
	// refused: the input deck it verified against, and the signed frame it
	// refused, whole. Everything a verdict needs travels in the complaint -
	// a peer re-runs the shuffle proof against the claimed input and names
	// whoever was wrong, so nothing answers a complaint and nothing needs
	// to.
	KindShuffleComplaint Kind = "shuffle_complaint"
)

// Message is the envelope every payload travels in.
type Message struct {
	// V is the schema version. It is repeated inside the payload as well as
	// in the frame so a stored message stays interpretable on its own,
	// without the frame it happened to arrive in.
	V     int             `json:"v"`
	Kind  Kind            `json:"kind"`
	Match string          `json:"match"`
	Body  json.RawMessage `json:"body"`
}

// Terms are what an invitation stated, carried so a peer can say "you and I
// read different invitations" rather than only "our memberships differ".
type Terms struct {
	Game       string `json:"game"`
	GameVer    int    `json:"gv"`
	SID        string `json:"sid"`
	BuyInAtoms uint64 `json:"buyin"`
	Seats      uint32 `json:"seats"`
	CSVBlocks  uint32 `json:"csv"`
	Until      uint32 `json:"until"`
}

// Join is one player's claim to a seat, and the bond that makes it cost
// something.
type Join struct {
	Key string `json:"key"` // hex compressed session pubkey
	// LogKey is the per-match key this player signs the action log with.
	//
	// It travels inside the join, and not as a message of its own, because
	// the join's signature covers it - so the join is the binding, there is
	// no second message to lose on a channel that loses messages, and a peer
	// that relays somebody else's join relays the binding with it.
	LogKey string `json:"logKey"`
	Sig    string `json:"sig"`
	// The bond's script travels rather than only its outpoint, because the
	// script is what says the coin is locked at all. Pop ties it to Key, so
	// a deposit cited in public cannot be lifted into another join.
	BondOutpoint string `json:"bondOutpoint"`
	BondScript   string `json:"bondScript"`
	BondPoP      string `json:"bondPop"`
}

// Roster is the table's seats and the keys their actions are signed with.
//
// Joins are what make it checkable. Carrying bare keys would let any member
// inject a key nobody joined with by burying it among real ones; carrying the
// signed joins means a receiver recomputes the seats itself and refuses the
// whole message if they disagree.
type Roster struct {
	Seats map[uint32]string `json:"seats"` // seat -> hex compressed session pubkey
	Joins []Join            `json:"joins,omitempty"`
	Terms *Terms            `json:"terms,omitempty"`
	// Roster, Signer and Sig make the claim attributable. Everyone
	// asserting the same membership is what lets a table form before its
	// deadline, so an unsigned assertion would let anybody manufacture
	// agreement and drive peers holding different join sets to bind
	// different memberships.
	Roster string `json:"roster,omitempty"`
	Signer string `json:"signer,omitempty"`
	Sig    string `json:"sig,omitempty"`
}

// Commit is one member binding itself to a membership, irrevocably.
type Commit struct {
	Roster string `json:"roster"` // hex roster hash
	Signer string `json:"signer"` // hex compressed session pubkey
	Sig    string `json:"sig"`
}

// Funded is one member pointing at the output that holds its stake.
type Funded struct {
	Seat     uint32 `json:"seat"`
	Outpoint string `json:"outpoint"`
	Signer   string `json:"signer"` // hex compressed session pubkey
	Sig      string `json:"sig"`
}

// Action is one signed log entry, in the same hex-rendered shape a transcript
// uses so a message and a stored transcript agree field for field.
type Action struct {
	Entry gamelog.TranscriptEntry `json:"entry"`
}

// Head is a seat's claim about the log head.
type Head struct {
	Seq    uint64 `json:"seq"`
	Hash   string `json:"hash"`
	Seat   uint32 `json:"seat"`
	Signer string `json:"signer"`
	Sig    string `json:"sig"`
}

// Resync asks for everything after a sequence number.
//
// It also says what the asker holds of the table's formation, because the same
// reconnection that loses log entries loses joins and commits, and those are
// not in the log. A peer short of one member's commit is waiting on a message
// nobody will send a second time, and would wait forever. Naming what it has
// keeps the answer to the difference rather than to the whole table: both lists
// are hex compressed session keys.
type Resync struct {
	After   uint64   `json:"after"`
	Joins   []string `json:"joins,omitempty"`
	Commits []string `json:"commits,omitempty"`
}

// ResyncReply carries the missing entries, in order.
//
// Joins and commits travel whole and signed, so a peer that answers with
// something it invented has the answer rejected rather than believed. That is
// the same property that lets a join be relayed by anyone at all: the receiver
// checks it instead of trusting whoever passed it along.
type ResyncReply struct {
	Entries []gamelog.TranscriptEntry `json:"entries"`
	Joins   []Join                    `json:"joins,omitempty"`
	Commits []Commit                  `json:"commits,omitempty"`
}

// Dispute is evidence that one seat signed two different things at one point in
// the log. Both entries travel whole, because the proof is the pair.
type Dispute struct {
	A gamelog.TranscriptEntry `json:"a"`
	B gamelog.TranscriptEntry `json:"b"`
	// Reason is for humans reading a log. Nothing acts on it: the proof is
	// the two entries, and a receiver that believed this field instead
	// would be trusting the accuser.
	Reason string `json:"reason,omitempty"`
}

// Encode wraps a body as a message ready to be framed, stamped with the
// caller's game protocol version.
func Encode(version int, kind Kind, matchID string, body any) ([]byte, error) {
	if matchID == "" {
		return nil, fmt.Errorf("message must name its match")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode %s body: %w", kind, err)
	}
	return json.Marshal(Message{V: version, Kind: kind, Match: matchID, Body: raw})
}

// Decode reads a message, refusing any not stamped with the caller's version.
// The body is left raw so a caller can dispatch on Kind and unmarshal only what
// it understands - a message for a kind this build has never heard of has to be
// skippable, not fatal.
func Decode(version int, blob []byte) (*Message, error) {
	var m Message
	if err := json.Unmarshal(blob, &m); err != nil {
		return nil, fmt.Errorf("decode message: %w", err)
	}
	if m.V != version {
		return nil, fmt.Errorf("message is schema version %d, this build speaks %d", m.V, version)
	}
	if m.Match == "" {
		return nil, fmt.Errorf("message names no match")
	}
	if m.Kind == "" {
		return nil, fmt.Errorf("message has no kind")
	}
	return &m, nil
}

// Into unmarshals a message body into v.
func (m *Message) Into(v any) error {
	if err := json.Unmarshal(m.Body, v); err != nil {
		return fmt.Errorf("decode %s body: %w", m.Kind, err)
	}
	return nil
}
