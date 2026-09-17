package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

// The pins below are literals, never derived from the code under test. The
// emitter's output is read by two parsers built from separate copies of the
// frame regex, so a drift here breaks tables between builds, not within one.

const (
	goldenFrame = `--gaming[v=2,game=poker,gv=5,sid=0123456789abcdef,mid=388d6729aa34aa0da30879de821236cddb5efc55e839bd40f530ad7c3a3e2a23,seq=1/1,exp=1783000000]--eyJhY3Rpb24iOiJmb2xkIn0=`

	goldenSampleEnvelopeSHA256 = "9214b258caa627ee38ea320805894f5ad1b2b3f55706eeaf72eb96ff4bee4374"
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

	if part != goldenFrame {
		t.Fatalf("the frame is %q, want the pinned %q", part, goldenFrame)
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
	if got, want := sidRE.String(), `^[0-9a-f]{1,32}$`; got != want {
		t.Fatalf("sidRE is %q, want the pinned %q", got, want)
	}
	if got, want := midRE.String(), `^[0-9a-f]{64}$`; got != want {
		t.Fatalf("midRE is %q, want the pinned %q", got, want)
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
