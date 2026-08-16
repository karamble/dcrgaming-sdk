package gamelog

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Transcript is a whole match's log, in a form that can be written to a file
// and handed to somebody.
//
// This is what the signing is for. A hand today is only as believable as
// whoever ran it, because they hold the sole account of it; a transcript can be
// checked by anyone, including someone who was not at the table and trusts
// nobody who was.
//
// The container itself is unsigned and does not need to be. Every entry carries
// its own signature and its link to the entry before it, so tampering with the
// container can only produce a transcript that fails to verify. It is JSON
// rather than the canonical encoding for the same reason: nothing here is
// signed, so readability wins, and the bytes that are signed are rebuilt from
// the fields during verification rather than trusted as transmitted.
type Transcript struct {
	Version int               `json:"version"`
	MatchID string            `json:"match_id"`
	Roster  map[uint32]string `json:"roster"` // seat -> hex compressed pubkey
	Entries []TranscriptEntry `json:"entries"`
}

// TranscriptEntry is one entry with its binary fields in hex, so a transcript
// can be read as well as verified.
type TranscriptEntry struct {
	Version  uint16 `json:"version"`
	PrevHash string `json:"prev_hash"`
	Seq      uint64 `json:"seq"`
	Hand     uint64 `json:"hand"`
	Street   string `json:"street"`
	Seat     uint32 `json:"seat"`
	Signer   string `json:"signer"`
	Action   string `json:"action"`
	Amount   int64  `json:"amount"`
	Height   uint32 `json:"height"`
	Sig      string `json:"sig"`
}

// Marshal renders the chain as a transcript.
func (c *Chain) Marshal() ([]byte, error) {
	t := Transcript{
		Version: 2,
		MatchID: c.matchID,
		Roster:  make(map[uint32]string, len(c.roster)),
		Entries: make([]TranscriptEntry, 0, len(c.entries)),
	}
	for seat, key := range c.roster {
		t.Roster[seat] = hex.EncodeToString(key)
	}
	for _, e := range c.entries {
		t.Entries = append(t.Entries, TranscriptEntry{
			Version:  e.Version,
			PrevHash: hex.EncodeToString(e.PrevHash[:]),
			Seq:      e.Seq,
			Hand:     e.Hand,
			Street:   e.Street.String(),
			Seat:     e.Seat,
			Signer:   hex.EncodeToString(e.Signer),
			Action:   string(e.Action),
			Amount:   e.Amount,
			Height:   e.Height,
			Sig:      hex.EncodeToString(e.Sig),
		})
	}
	return json.MarshalIndent(t, "", "  ")
}

// Unmarshal rebuilds a chain from a transcript, verifying every entry on the
// way. A transcript that does not verify does not produce a chain.
func Unmarshal(blob []byte) (*Chain, error) {
	var t Transcript
	if err := json.Unmarshal(blob, &t); err != nil {
		return nil, fmt.Errorf("decode transcript: %w", err)
	}
	if t.Version != 2 {
		return nil, fmt.Errorf("transcript version %d, want 2", t.Version)
	}

	roster := make(Roster, len(t.Roster))
	for seat, keyHex := range t.Roster {
		key, err := hex.DecodeString(keyHex)
		if err != nil {
			return nil, fmt.Errorf("seat %d key: %w", seat, err)
		}
		roster[seat] = key
	}
	c, err := NewChain(t.MatchID, roster)
	if err != nil {
		return nil, err
	}

	for i, te := range t.Entries {
		e, err := te.entry()
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		// Append re-checks the signature and the linkage, so a transcript
		// is verified by the same code that accepted the entries live.
		if err := c.Append(e); err != nil {
			return nil, fmt.Errorf("entry %d (seq %d): %w", i, te.Seq, err)
		}
	}
	return c, nil
}

func (te TranscriptEntry) entry() (*Entry, error) {
	prev, err := hex.DecodeString(te.PrevHash)
	if err != nil || len(prev) != 32 {
		return nil, fmt.Errorf("bad prev_hash")
	}
	signer, err := hex.DecodeString(te.Signer)
	if err != nil {
		return nil, fmt.Errorf("bad signer: %w", err)
	}
	sig, err := hex.DecodeString(te.Sig)
	if err != nil {
		return nil, fmt.Errorf("bad sig: %w", err)
	}
	street, err := parseStreet(te.Street)
	if err != nil {
		return nil, err
	}

	e := &Entry{
		Version: te.Version,
		Seq:     te.Seq,
		Hand:    te.Hand,
		Street:  street,
		Seat:    te.Seat,
		Signer:  signer,
		Action:  Action(te.Action),
		Amount:  te.Amount,
		Height:  te.Height,
		Sig:     sig,
	}
	copy(e.PrevHash[:], prev)
	return e, nil
}

func parseStreet(s string) (Street, error) {
	switch s {
	case "preflop":
		return StreetPreFlop, nil
	case "flop":
		return StreetFlop, nil
	case "turn":
		return StreetTurn, nil
	case "river":
		return StreetRiver, nil
	}
	return 0, fmt.Errorf("unknown street %q", s)
}

// Transcript renders one entry in the shape a transcript carries, so a message
// and a stored history agree field for field.
func (e *Entry) Transcript() TranscriptEntry {
	return TranscriptEntry{
		Version:  e.Version,
		PrevHash: hex.EncodeToString(e.PrevHash[:]),
		Seq:      e.Seq,
		Hand:     e.Hand,
		Street:   e.Street.String(),
		Seat:     e.Seat,
		Signer:   hex.EncodeToString(e.Signer),
		Action:   string(e.Action),
		Amount:   e.Amount,
		Height:   e.Height,
		Sig:      hex.EncodeToString(e.Sig),
	}
}

// Entry rebuilds an entry from its transcript form. The signature is not
// checked here; Chain.Append is where that belongs, because it needs the roster.
func (te TranscriptEntry) Entry() (*Entry, error) { return te.entry() }
