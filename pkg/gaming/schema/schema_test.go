package schema

import (
	"encoding/json"
	"testing"

	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
	"github.com/karamble/dcrgaming-sdk/pkg/gamelog"
)

const testMatch = "table1|sess1"

// A message has to survive the trip and mean the same thing on arrival.
func TestActionRoundTrips(t *testing.T) {
	priv, err := forfeit.NewLogKey(testMatch)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	roster := gamelog.Roster{
		0: priv.Public().SerializeCompressed(),
		1: mustKey(t),
	}
	c, err := gamelog.NewChain(testMatch, roster)
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	e := c.Next(0, 1, gamelog.StreetFlop, gamelog.ActionBet, 500)
	if err := e.Sign(priv); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := c.Append(e); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Take the entry through the transcript shape the schema uses.
	blob, err := c.Marshal()
	if err != nil {
		t.Fatalf("marshal chain: %v", err)
	}
	var tr gamelog.Transcript
	if err := json.Unmarshal(blob, &tr); err != nil {
		t.Fatalf("read transcript: %v", err)
	}

	msg, err := Encode(5, KindAction, testMatch, Action{Entry: tr.Entries[0]})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := Decode(5, msg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != KindAction || got.Match != testMatch || got.V != 5 {
		t.Fatalf("message header did not survive: %+v", got)
	}

	var back Action
	if err := got.Into(&back); err != nil {
		t.Fatalf("into: %v", err)
	}
	if back.Entry.Sig != tr.Entries[0].Sig || back.Entry.Seq != tr.Entries[0].Seq {
		t.Fatal("the signed entry did not survive the round trip")
	}
}

// A body this build does not understand must be skippable, not fatal, or one
// new message kind would break every existing client.
func TestUnknownKindDecodesAndIsSkippable(t *testing.T) {
	raw, err := json.Marshal(Message{
		V: 5, Kind: "something_new", Match: testMatch, Body: json.RawMessage(`{"x":1}`),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	m, err := Decode(5, raw)
	if err != nil {
		t.Fatalf("an unknown kind must still decode: %v", err)
	}
	if m.Kind != "something_new" {
		t.Fatalf("kind is %q", m.Kind)
	}
}

func TestDecodeRejectsUnusableMessages(t *testing.T) {
	cases := map[string]string{
		"not json":      `{`,
		"wrong version": `{"v":99,"kind":"action","match":"t","body":{}}`,
		"no match":      `{"v":1,"kind":"action","match":"","body":{}}`,
		"no kind":       `{"v":1,"kind":"","match":"t","body":{}}`,
	}
	for name, blob := range cases {
		if _, err := Decode(5, []byte(blob)); err == nil {
			t.Errorf("%s should be refused", name)
		}
	}
}

func TestEncodeRequiresAMatch(t *testing.T) {
	if _, err := Encode(5, KindHead, "", Head{}); err == nil {
		t.Fatal("a message must name the table it belongs to")
	}
}

// Identity is never a payload field: the sender's uid is the authenticated
// identity, and a field claiming one could only be a way to lie about it.
func TestNoMessageCarriesAnIdentityField(t *testing.T) {
	for name, v := range map[string]any{
		"join":         Join{},
		"roster":       Roster{},
		"commit":       Commit{},
		"terms":        Terms{},
		"action":       Action{},
		"head":         Head{},
		"resync":       Resync{},
		"resync_reply": ResyncReply{},
		"dispute":      Dispute{},
	} {
		blob, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		var fields map[string]any
		if err := json.Unmarshal(blob, &fields); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, banned := range []string{"uid", "sender", "from", "identity", "nick"} {
			if _, found := fields[banned]; found {
				t.Errorf("%s carries an identity field %q", name, banned)
			}
		}
	}
}

func mustKey(t *testing.T) []byte {
	t.Helper()
	priv, err := forfeit.NewLogKey(testMatch)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return priv.Public().SerializeCompressed()
}
