package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// The pins below are literals, never derived from the code under test. The
// emitter's output is read by two parsers built from separate copies of the
// frame regex, so a drift here breaks tables between builds, not within one.

const (
	// The single-part frame Encode produces for the fixed inputs below,
	// split around the one field that is drawn fresh per message.
	goldenFramePrefix = `--gaming[v=1,game=poker,gv=5,sid=0123456789abcdef,mid=`
	goldenFrameSuffix = `,seq=1/1,exp=1783000000]--eyJhY3Rpb24iOiJmb2xkIn0=`

	goldenSampleEnvelopeSHA256 = "76d3617bdd19daae3c24649f3984c41cf50760ba1669c2f349f3fe8f407483f5"
)

func TestTheEmitterOutputIsPinned(t *testing.T) {
	parts, err := Encode("poker", 5, "0123456789abcdef",
		[]byte(`{"action":"fold"}`), time.Unix(1783000000, 0), 0)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1", len(parts))
	}
	part := parts[0]

	if !strings.HasPrefix(part, goldenFramePrefix) {
		t.Fatalf("the frame is %q, want the pinned prefix %q", part, goldenFramePrefix)
	}
	if len(part) < len(goldenFramePrefix)+16 {
		t.Fatalf("the frame is %q, too short to carry a message id", part)
	}
	mid := part[len(goldenFramePrefix) : len(goldenFramePrefix)+16]
	// The message id is the one fresh field; its shape is checked here
	// against a literal alphabet rather than the package's own idRE.
	for _, r := range mid {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("the message id %q is not 16 lowercase hex", mid)
		}
	}
	if want := goldenFramePrefix + mid + goldenFrameSuffix; part != want {
		t.Fatalf("the frame is %q, want the pinned %q", part, want)
	}

	if !partRE.MatchString(part) {
		t.Fatal("the emitter produced a frame its own parser regex does not match")
	}
}

// The frame regex is copied into brclientd and dcrpulse; those copies are
// found by eye, so this pin is the alarm that says go look at them.
func TestTheFrameRegexSourcesArePinned(t *testing.T) {
	if got, want := partRE.String(), `^--gaming\[([^\]]*)\]--([A-Za-z0-9+/=\s]*)$`; got != want {
		t.Fatalf("partRE is %q, want the pinned %q - the same source is copied into brclientd and dcrpulse", got, want)
	}
	if got, want := idRE.String(), `^[0-9a-f]{1,32}$`; got != want {
		t.Fatalf("idRE is %q, want the pinned %q", got, want)
	}
	if got, want := gameRE.String(), `^[a-z0-9][a-z0-9_-]*$`; got != want {
		t.Fatalf("gameRE is %q, want the pinned %q", got, want)
	}
}

func TestTheSampleEnvelopeIsPinned(t *testing.T) {
	sum := sha256.Sum256([]byte(SampleEnvelope))
	if got := hex.EncodeToString(sum[:]); got != goldenSampleEnvelopeSHA256 {
		t.Fatalf("SampleEnvelope hashes to %s, want the pinned %s - hosts build filter guards against these exact bytes", got, goldenSampleEnvelopeSHA256)
	}
}
