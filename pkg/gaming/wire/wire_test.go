package wire

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

const testSID = "0123456789abcdef"

func TestEncodeRoundTripsASinglePart(t *testing.T) {
	payload := []byte(`{"kind":"action"}`)
	parts, err := Encode("poker", 1, testSID, payload, time.Unix(1783000000, 0), 0)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1", len(parts))
	}

	p, ok := Parse(parts[0])
	if !ok {
		t.Fatal("a frame we produced must parse")
	}
	if p.Game != "poker" || p.GameVer != 1 || p.SID != testSID {
		t.Fatalf("attributes did not survive: %+v", p)
	}
	if !bytes.Equal(p.Chunk, payload) {
		t.Fatalf("payload did not survive")
	}
}

func TestChunkedMessageReassembles(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefgh"), 100) // 800 bytes
	parts, err := Encode("poker", 1, testSID, payload, time.Time{}, 100)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(parts) != 8 {
		t.Fatalf("got %d parts, want 8", len(parts))
	}

	// Group chat gives no ordering, so out-of-order arrival is the normal
	// case rather than an edge one.
	order := []int{5, 0, 7, 2, 1, 6, 3, 4}
	a := NewAssembler(AssemblerConfig{})
	now := time.Unix(1_700_000_000, 0)

	var got []byte
	for _, i := range order {
		p, ok := Parse(parts[i])
		if !ok {
			t.Fatalf("part %d did not parse", i)
		}
		out, err := a.Add("gc/alice", p, now)
		if err != nil {
			t.Fatalf("add part %d: %v", i, err)
		}
		if out != nil {
			got = out
		}
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("reassembled payload differs from what was sent")
	}
	if a.Pending() != 0 {
		t.Fatalf("assembler still holds %d partial messages", a.Pending())
	}
}

// Anything that is not exactly a frame is somebody talking. Getting this wrong
// means a person's message disappearing.
func TestParseLeavesChatAlone(t *testing.T) {
	frame := SampleEnvelope
	for _, text := range []string{
		"",
		"hello",
		"look at " + frame,
		frame + " what do you think",
		"--mcp[v=1,sid=ab,mid=cd,seq=1/1,exp=0]--QUJD",
		"--embed[type=image/png,data=AAAA]--",
		// A bare token is malformed rather than an unknown extension.
		"--gaming[v=1,game=poker,gv=1,sid=ab,mid=cd,seq=1/1,nonsense]--QUJD",
		// Empty payloads carry nothing.
		"--gaming[v=1,game=poker,gv=1,sid=ab,mid=cd,seq=1/1,exp=0]--",
		// Payload that is not base64.
		"--gaming[v=1,game=poker,gv=1,sid=ab,mid=cd,seq=1/1,exp=0]--not base64 here",
	} {
		if _, ok := Parse(text); ok {
			t.Errorf("ordinary text parsed as a frame: %q", text)
		}
	}
}

func TestParseRejectsMalformedAttributes(t *testing.T) {
	cases := map[string]string{
		"no version":       "--gaming[game=poker,gv=1,sid=ab,mid=cd,seq=1/1]--QUJD",
		"future framing":   "--gaming[v=2,game=poker,gv=1,sid=ab,mid=cd,seq=1/1]--QUJD",
		"no game":          "--gaming[v=1,gv=1,sid=ab,mid=cd,seq=1/1]--QUJD",
		"game not a key":   "--gaming[v=1,game=Poker!,gv=1,sid=ab,mid=cd,seq=1/1]--QUJD",
		"sid not hex":      "--gaming[v=1,game=poker,gv=1,sid=zz,mid=cd,seq=1/1]--QUJD",
		"seq beyond total": "--gaming[v=1,game=poker,gv=1,sid=ab,mid=cd,seq=3/2]--QUJD",
		"seq zero":         "--gaming[v=1,game=poker,gv=1,sid=ab,mid=cd,seq=0/2]--QUJD",
		"too many parts":   "--gaming[v=1,game=poker,gv=1,sid=ab,mid=cd,seq=1/999]--QUJD",
	}
	for name, text := range cases {
		if _, ok := Parse(text); ok {
			t.Errorf("%s should not parse", name)
		}
	}
}

// A game this build has never heard of still has to be recognised as protocol
// traffic, or an old client would show a new game's frames as chat.
func TestUnknownGameStillParses(t *testing.T) {
	p, ok := Parse("--gaming[v=1,game=chess,gv=9,sid=ab,mid=cd,seq=1/1,exp=0]--QUJD")
	if !ok {
		t.Fatal("a frame for another game must still be recognised")
	}
	if p.Game != "chess" {
		t.Fatalf("routing key is %q", p.Game)
	}
}

func TestExpiredPartsAreDroppedBeforeAnyState(t *testing.T) {
	parts, err := Encode("poker", 1, testSID, bytes.Repeat([]byte("x"), 300), time.Unix(1000, 0), 100)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	p, ok := Parse(parts[0])
	if !ok {
		t.Fatal("parse")
	}

	a := NewAssembler(AssemblerConfig{})
	if _, err := a.Add("gc/alice", p, time.Unix(2000, 0)); err != ErrExpired {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
	if a.Pending() != 0 {
		t.Fatal("an expired part must not create reassembly state")
	}
}

// Frames are invisible by design, so unbounded buffering would be a silent way
// to exhaust memory.
func TestAssemblerBoundsWhatOneSenderCanHold(t *testing.T) {
	a := NewAssembler(AssemblerConfig{MaxPendingPerSender: 2})
	now := time.Unix(1_700_000_000, 0)

	// Each message starts but never finishes.
	started := 0
	for i := 0; i < 5; i++ {
		parts, err := Encode("poker", 1, testSID, bytes.Repeat([]byte("y"), 300), time.Time{}, 100)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		p, ok := Parse(parts[0])
		if !ok {
			t.Fatal("parse")
		}
		if _, err := a.Add("gc/flooder", p, now); err == nil {
			started++
		}
	}
	if started != 2 {
		t.Fatalf("sender held %d partial messages, cap was 2", started)
	}
}

func TestAssemblerIgnoresDuplicatesAndDropsInconsistentTotals(t *testing.T) {
	parts, _ := Encode("poker", 1, testSID, bytes.Repeat([]byte("z"), 300), time.Time{}, 100)
	p0, _ := Parse(parts[0])
	now := time.Unix(1_700_000_000, 0)

	a := NewAssembler(AssemblerConfig{})
	if _, err := a.Add("gc/alice", p0, now); err != nil {
		t.Fatalf("add: %v", err)
	}
	// The same part again is not new information.
	if out, err := a.Add("gc/alice", p0, now); err != nil || out != nil {
		t.Fatalf("a duplicate part should be ignored, got %v %v", out, err)
	}

	// A part claiming a different total belongs to a different message and
	// cannot be merged with this one.
	conflicting := *p0
	conflicting.Seq, conflicting.Total = 1, 2
	if _, err := a.Add("gc/alice", &conflicting, now); err == nil {
		t.Fatal("a part with a different total must be refused")
	}
}

func TestPartialMessagesExpire(t *testing.T) {
	parts, _ := Encode("poker", 1, testSID, bytes.Repeat([]byte("w"), 300), time.Time{}, 100)
	p0, _ := Parse(parts[0])

	a := NewAssembler(AssemblerConfig{Timeout: time.Minute})
	start := time.Unix(1_700_000_000, 0)
	if _, err := a.Add("gc/alice", p0, start); err != nil {
		t.Fatalf("add: %v", err)
	}
	if a.Pending() != 1 {
		t.Fatal("expected one partial message")
	}

	// Any later part sweeps whatever went quiet.
	p1, _ := Parse(parts[1])
	if _, err := a.Add("gc/bob", p1, start.Add(2*time.Minute)); err != nil {
		t.Fatalf("add: %v", err)
	}
	if a.Pending() != 1 {
		t.Fatalf("stale message was not evicted; pending=%d", a.Pending())
	}
}

// A dispute notice must outlive the window an honest player needs to boot and
// answer, or store-and-forward plus a short deadline would quietly remove it.
func TestDisputeOutlivesTurnTraffic(t *testing.T) {
	if ClassDispute.TTL() <= ClassState.TTL() || ClassState.TTL() <= ClassTurn.TTL() {
		t.Fatal("dispute must outlive state sync, which must outlive turn traffic")
	}
	if ClassTurn.TTL() > 5*time.Minute {
		t.Fatal("turn traffic must go stale quickly or a raise could land after settlement")
	}
	if ClassDispute.TTL() < time.Hour {
		t.Fatal("a dispute notice must survive a player rebooting")
	}
}

// Formation traffic is gated by a block height, not a clock, so its wire
// deadline only has to outlive a relay backlog - and ten minutes did not: a
// join expired in a peer's queue, and the table formed on one box and aborted
// on the other.
func TestFormationOutlivesABacklog(t *testing.T) {
	if ClassForm.TTL() < time.Hour {
		t.Fatal("a formation frame must survive a relay backlog measured in minutes, with room")
	}

	// A join eleven minutes old - dead as state sync - is still a join.
	sent := time.Unix(1000, 0)
	frames, err := Encode("poker", 1, "0011223344556677", []byte("join"),
		ClassForm.Deadline(sent), 0)
	if err != nil || len(frames) != 1 {
		t.Fatalf("encode: %v (%d frames)", err, len(frames))
	}
	p, ok := Parse(frames[0])
	if !ok {
		t.Fatal("did not parse")
	}
	a := NewAssembler(AssemblerConfig{})
	if _, err := a.Add("gc/alice", p, sent.Add(11*time.Minute)); err != nil {
		t.Fatalf("an 11 minute old formation frame was refused: %v", err)
	}
}

func TestSampleEnvelopeIsAFrame(t *testing.T) {
	if !IsEnvelope(SampleEnvelope) {
		t.Fatal("SampleEnvelope must parse, or filter guards built on it are inert")
	}
}

func TestEncodeRefusesUnsendableInput(t *testing.T) {
	for name, fn := range map[string]func() error{
		"no payload": func() error { _, err := Encode("poker", 1, testSID, nil, time.Time{}, 0); return err },
		"bad game":   func() error { _, err := Encode("Poker!", 1, testSID, []byte("x"), time.Time{}, 0); return err },
		"bad sid":    func() error { _, err := Encode("poker", 1, "zz", []byte("x"), time.Time{}, 0); return err },
		"over max": func() error {
			_, err := Encode("poker", 1, testSID, bytes.Repeat([]byte("x"), 100), time.Time{}, 1)
			return err
		},
	} {
		if err := fn(); err == nil {
			t.Errorf("%s should be refused", name)
		}
	}
}

func TestNewIDIsHex(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	if len(id) != 16 || strings.ContainsFunc(id, func(r rune) bool {
		return !strings.ContainsRune("0123456789abcdef", r)
	}) {
		t.Fatalf("id %q is not 16 lowercase hex", id)
	}
}
