package gamingpb

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"testing"

	"google.golang.org/protobuf/proto"
)

// contractSHA256 is the bridge contract this game speaks, byte for byte.
//
// dcrpulse generates the stubs and pins its own copy of the contract to the
// same number. That is the whole point of pinning it in every repo that
// carries a copy: the wire is one artifact, and a copy that drifted would
// still compile on both sides and still generate stubs, then fail at a live
// table with a field nobody sent.
//
// Updating it is a deliberate act on every side at once. If this test fails,
// the question is not "what is the new hash" but "which repo changed the wire,
// and has every other copy been given the same change".
const contractSHA256 = "98681ea0d77a2a3f4e50f0ba58e641d7dd7e422809558b280e976193b843d485"

func TestTheWireContractMatchesTheBridge(t *testing.T) {
	raw, err := os.ReadFile("gaming_bridge.proto")
	if err != nil {
		t.Fatalf("read the contract: %v", err)
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != contractSHA256 {
		t.Fatalf("the contract in this repo is not the one dcrpulse serves.\n"+
			"  here: %s\n  bridge: %s\n"+
			"Copy dcrpulse's dashboard/internal/gamingpb/gaming_bridge.proto over this one and "+
			"regenerate, or take the change back to dcrpulse first - do not edit the hash.",
			hex.EncodeToString(sum[:]), contractSHA256)
	}
}

// The generated stubs have to match the contract they claim to come from.
//
// The hash above guards the source; this guards the output, which is what
// actually gets compiled. Together they catch stubs regenerated from a
// different file, or committed without regenerating at all.
func TestTheServiceOffersExactlyTheseCalls(t *testing.T) {
	want := []string{
		// handshake
		"Hello",
		// the bridge->game channel and its reply half
		"Respond", "ReportState",
		// money - the only verbs where the bridge forms an opinion
		"RequestSpend", "SpendStatus", "Broadcast",
		// frames and chain
		"SendFrame", "ChainTip", "BlockHash", "Outpoint",
	}
	got := make([]string, 0, len(BridgeService_ServiceDesc.Methods))
	for _, m := range BridgeService_ServiceDesc.Methods {
		got = append(got, m.MethodName)
	}
	sort.Strings(want)
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("the service offers %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the service offers %v, want %v", got, want)
		}
	}

	// Subscribe is the one stream, and it is server-side only: the bridge
	// pushes down the connection the game opened, and never dials a game.
	if len(BridgeService_ServiceDesc.Streams) != 1 {
		t.Fatalf("the service has %d streams, want exactly Subscribe",
			len(BridgeService_ServiceDesc.Streams))
	}
	s := BridgeService_ServiceDesc.Streams[0]
	if s.StreamName != "Subscribe" {
		t.Fatalf("the stream is %q, want Subscribe", s.StreamName)
	}
	if s.ClientStreams {
		t.Error("Subscribe accepts a client stream; the game's calls are unary")
	}
	if !s.ServerStreams {
		t.Error("Subscribe does not stream from the bridge, so nothing can be pushed")
	}
}

// The registry key and every gRPC :path value derive from this recorded path;
// a regen under a different --proto_path moves them without touching the .proto.
func TestTheDescriptorPathIsPinned(t *testing.T) {
	if got := File_gaming_bridge_proto.Path(); got != "gaming_bridge.proto" {
		t.Fatalf("the descriptor records its source as %q, want the bare gaming_bridge.proto", got)
	}
}

// No message carries the caller's own name.
//
// A game presents a credential and the bridge decides which game it is. A field
// it could fill in would be a second answer to that question, and the two could
// disagree.
func TestNoRequestNamesItsOwnGame(t *testing.T) {
	for _, m := range []proto.Message{
		&RequestSpendRequest{},
		&SendFrameRequest{},
		&BroadcastRequest{},
		&SpendStatusRequest{},
		&SubscribeRequest{},
	} {
		d := m.ProtoReflect().Descriptor()
		for i := 0; i < d.Fields().Len(); i++ {
			switch name := string(d.Fields().Get(i).Name()); name {
			case "game", "game_id":
				t.Errorf("%s carries %q, so a game could name itself rather than "+
					"being told what it is", d.Name(), name)
			}
		}
	}
}
