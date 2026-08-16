// Package wire frames game protocol traffic for carriage over Bison Relay.
//
// A frame is one message body:
//
//	--gaming[v=1,game=poker,gv=1,sid=<hex>,mid=<hex>,seq=<n>/<total>,exp=<unix>]--<base64>
//
// It is the sibling of brmcp's --mcp[…]--, and copies its framing deliberately:
// the anchored whole-body match, the base64-restricted payload, order-
// independent attributes, tolerance of unknown keys, and bounded reassembly are
// all load-bearing and all already proven there.
//
// Two things differ, and both come from a table being more than two people.
//
// It rides a group chat. Bison Relay fans a group message out as separate
// one-to-one messages, so there is no relay point, no shared transcript and no
// ordering - which means this layer must never be asked to establish any. It
// delivers bytes and says who they came from. Sequencing, agreement and proof
// of who said what belong to the payload, which is why pkg/gamelog signs and
// chains its entries rather than trusting arrival order.
//
// And it is a namespace over games rather than one tag per game. A client that
// receives a game it does not implement must ignore the frame and must not show
// it as chat: that single rule is what lets a new game appear without every
// existing client needing to be taught about it first.
package wire

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Version is the framing version. It is separate from a game's own protocol
// version on purpose: a breaking change to poker's rules must not invalidate
// another game's traffic, and brmcp gets away with a single version only
// because it carries one kind of payload.
const Version = 1

const (
	// DefaultChunkSize is the raw bytes per part, before base64. Base64
	// expands by 4/3, so parts stay well under Bison Relay's 1 MiB
	// routed-message floor with room for the tag.
	DefaultChunkSize = 200 * 1024

	// MaxParts bounds one message at ~12.8 MiB raw.
	MaxParts = 64

	// MaxGameLen bounds the routing key so a malformed tag cannot carry an
	// unbounded string into a router's map.
	MaxGameLen = 32
)

var (
	// partRE matches a whole message body that is one frame. Anchoring is
	// what keeps a human who mentions --gaming[ in conversation from having
	// their message silently swallowed, and the payload alphabet excludes
	// brackets so a body cannot smuggle a second tag past the match.
	partRE = regexp.MustCompile(`^--gaming\[([^\]]*)\]--([A-Za-z0-9+/=\s]*)$`)

	idRE   = regexp.MustCompile(`^[0-9a-f]{1,32}$`)
	gameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// SampleEnvelope is a representative frame. Hosts that let users define content
// filters must refuse any rule matching it: Bison Relay applies filters on the
// live receive path, so a rule that swallows these drops a player out of a hand
// with funds escrowed, looking exactly like walking away.
const SampleEnvelope = `--gaming[v=1,game=poker,gv=1,sid=0123456789abcdef,mid=0123456789abcdef,seq=1/1,exp=1783000000]--eyJhY3Rpb24iOiJmb2xkIn0=`

// Part is one frame of a possibly chunked message.
type Part struct {
	Version int
	Game    string // routing key: which game this belongs to
	GameVer int    // the game's own protocol version
	SID     string // session: which table
	MID     string // message: which logical message these chunks form
	Seq     int    // 1-based
	Total   int
	Exp     int64 // unix seconds; 0 means no deadline
	Chunk   []byte
}

// Expired reports whether the part's deadline has passed.
func (p *Part) Expired(now time.Time) bool {
	return p.Exp > 0 && now.Unix() > p.Exp
}

// Parse reads a message body as a frame. The second return is false for
// anything that is not one, which includes ordinary chat.
func Parse(text string) (*Part, bool) {
	m := partRE.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return nil, false
	}

	p := &Part{}
	seenVersion := false
	for _, tok := range strings.Split(m[1], ",") {
		if tok == "" {
			continue
		}
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			// A bare token is malformed, not an unknown extension.
			return nil, false
		}
		switch k {
		case "v":
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, false
			}
			p.Version = n
			seenVersion = true
		case "game":
			p.Game = v
		case "gv":
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, false
			}
			p.GameVer = n
		case "sid":
			p.SID = v
		case "mid":
			p.MID = v
		case "seq":
			a, b, ok := strings.Cut(v, "/")
			if !ok {
				return nil, false
			}
			n, err := strconv.Atoi(a)
			if err != nil {
				return nil, false
			}
			total, err := strconv.Atoi(b)
			if err != nil {
				return nil, false
			}
			p.Seq, p.Total = n, total
		case "exp":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, false
			}
			p.Exp = n
		default:
			// Unknown keys are ignored so a newer sender does not break
			// an older receiver.
		}
	}

	// The framing version is checked, but the game and its version are not:
	// a client must be able to recognise - and therefore hide - traffic for
	// a game it has never heard of.
	if !seenVersion || p.Version != Version {
		return nil, false
	}
	if !gameRE.MatchString(p.Game) || len(p.Game) > MaxGameLen {
		return nil, false
	}
	if !idRE.MatchString(p.SID) || !idRE.MatchString(p.MID) {
		return nil, false
	}
	if p.Seq < 1 || p.Total < 1 || p.Seq > p.Total || p.Total > MaxParts {
		return nil, false
	}

	chunk, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(m[2]), ""))
	if err != nil || len(chunk) == 0 {
		return nil, false
	}
	p.Chunk = chunk
	return p, true
}

// IsEnvelope reports whether a message body is game protocol traffic. Hosts use
// it to keep frames out of chat without having to understand any game.
func IsEnvelope(text string) bool {
	_, ok := Parse(text)
	return ok
}

// Encode splits a payload into frames.
func Encode(game string, gameVer int, sid string, payload []byte, exp time.Time, chunkSize int) ([]string, error) {
	if !gameRE.MatchString(game) || len(game) > MaxGameLen {
		return nil, fmt.Errorf("game %q is not a valid routing key", game)
	}
	if !idRE.MatchString(sid) {
		return nil, fmt.Errorf("session id %q is not 1-32 lowercase hex", sid)
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("nothing to send")
	}
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	total := (len(payload) + chunkSize - 1) / chunkSize
	if total > MaxParts {
		return nil, fmt.Errorf("payload needs %d parts, over the %d part limit", total, MaxParts)
	}
	mid, err := NewID()
	if err != nil {
		return nil, err
	}

	var expUnix int64
	if !exp.IsZero() {
		expUnix = exp.Unix()
	}

	out := make([]string, 0, total)
	for i := 0; i < total; i++ {
		end := (i + 1) * chunkSize
		if end > len(payload) {
			end = len(payload)
		}
		b64 := base64.StdEncoding.EncodeToString(payload[i*chunkSize : end])
		out = append(out, fmt.Sprintf("--gaming[v=%d,game=%s,gv=%d,sid=%s,mid=%s,seq=%d/%d,exp=%d]--%s",
			Version, game, gameVer, sid, mid, i+1, total, expUnix, b64))
	}
	return out, nil
}
