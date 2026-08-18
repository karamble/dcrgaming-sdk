// Package identity is a game's persisted key material: one 32-byte seed from
// which every session, log and bond key is derived, so a restarted daemon
// reproduces the keys the escrow scripts already name.
//
// It holds no Bison Relay identity and no wallet key. The seed is generated
// here and never sent anywhere; what it produces are keys that sign, and
// nothing that receives. The package is game-agnostic: the caller names the
// domain tags and any per-table session id, and the derivation is
// HMAC-BLAKE256(seed, tag||sid).
package identity

import (
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/txscript/v4/stdaddr"

	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

// Identity is the only thing a game process keeps between runs.
type Identity struct {
	mu   sync.Mutex
	dir  string
	seed []byte

	// bondOutpoint is a one-per-identity bond deposit, for games that post a
	// single standing bond rather than one per table. Games with per-table
	// bonds leave it empty and record their outpoints elsewhere.
	bondOutpoint string
	// payout is where a game wants coin sent when a table owes it something it
	// did not pay for itself. Set by the operator, not derived: this process
	// holds no wallet. Optional; a game may keep its payout elsewhere.
	payout string
}

type identityFile struct {
	SeedHex      string `json:"seed_hex"`
	BondOutpoint string `json:"bond_outpoint,omitempty"`
	Payout       string `json:"payout,omitempty"`
}

// stateMarks are the artifacts whose presence beside a missing identity means a
// lost seed rather than a first run. The set is deliberately conservative: it
// names game state that funded coin depends on, so the guard fires exactly when
// a fresh seed would strand something and never on a clean first connect.
var stateMarks = []string{"sessions", "hands", "spends", "seatbonds.json", "evidence"}

// Load reads the seed under dir, creating one the first time.
func Load(dir string) (*Identity, error) {
	path := filepath.Join(dir, "identity.json")

	blob, err := os.ReadFile(path)
	switch {
	case err == nil:
		var f identityFile
		if err := json.Unmarshal(blob, &f); err != nil {
			return nil, fmt.Errorf("read identity: %w", err)
		}
		seed, err := hex.DecodeString(strings.TrimSpace(f.SeedHex))
		if err != nil || len(seed) != 32 {
			return nil, fmt.Errorf("identity seed is not 32 bytes of hex")
		}
		return &Identity{dir: dir, seed: seed, bondOutpoint: f.BondOutpoint, payout: f.Payout}, nil
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("read identity: %w", err)
	}

	// A directory holding a game's state but no identity is one whose seed has
	// been lost. Generating here would quietly make a new player while the
	// tables and coin belonging to the old one stayed on chain with nothing
	// left that can sign for them.
	for _, mark := range stateMarks {
		if hasMark(dir, mark) {
			return nil, fmt.Errorf("%s holds %s but no identity.json; refusing to generate a new "+
				"seed here, because coin held by the player this directory belongs to could no "+
				"longer be signed for", dir, mark)
		}
	}

	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, fmt.Errorf("generate seed: %w", err)
	}
	seed := priv.Serialize()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	out, err := json.Marshal(identityFile{SeedHex: hex.EncodeToString(seed)})
	if err != nil {
		return nil, err
	}
	// Write and rename, so a crash leaves either no identity or a whole one.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, fmt.Errorf("write identity: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("write identity: %w", err)
	}
	return &Identity{dir: dir, seed: seed}, nil
}

// hasMark reports whether dir holds an exact-named artifact, or one whose name
// carries a per-match suffix (spends-<id>.json, evidence-<id>.json).
func hasMark(dir, mark string) bool {
	if _, err := os.Stat(filepath.Join(dir, mark)); err == nil {
		return true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), mark+"-") {
			return true
		}
	}
	return false
}

// DeriveKey is the one primitive: HMAC-BLAKE256(seed, tag||sid). An empty sid
// derives a key bound to the tag alone.
func (id *Identity) DeriveKey(tag, sid string) (*secp256k1.PrivateKey, error) {
	if strings.TrimSpace(tag) == "" {
		return nil, fmt.Errorf("a derived key needs a domain tag")
	}
	// tag and sid are concatenated with no length framing, so callers must use
	// prefix-free domain tags (a trailing /vN suffices) and fixed-length sids;
	// otherwise two distinct (tag, sid) pairs could derive the same key.
	mac := hmac.New(blake256.New, id.seed)
	mac.Write([]byte(tag))
	if sid != "" {
		mac.Write([]byte(sid))
	}
	sum := mac.Sum(nil)
	if len(sum) != 32 {
		return nil, fmt.Errorf("derived %d bytes, want 32", len(sum))
	}
	return secp256k1.PrivKeyFromBytes(sum), nil
}

// SeatTags names the three domain tags one table's keys are derived under.
type SeatTags struct {
	Session string
	Log     string
	Bond    string
}

// SeatKeys derives the three per-table keys for one session id.
func (id *Identity) SeatKeys(tags SeatTags, sid string) (session, logKey, bond *secp256k1.PrivateKey, err error) {
	if strings.TrimSpace(sid) == "" {
		return nil, nil, nil, fmt.Errorf("no session to derive keys for")
	}
	if session, err = id.DeriveKey(tags.Session, sid); err != nil {
		return nil, nil, nil, err
	}
	if logKey, err = id.DeriveKey(tags.Log, sid); err != nil {
		return nil, nil, nil, err
	}
	if bond, err = id.DeriveKey(tags.Bond, sid); err != nil {
		return nil, nil, nil, err
	}
	return session, logKey, bond, nil
}

// Credentials derives the three per-table keys and returns them in a
// membership.Credentials. BondOutpoint and BondScript are left empty for the
// caller to fill once the bond has funded on chain.
func (id *Identity) Credentials(tags SeatTags, sid string) (membership.Credentials, error) {
	session, logKey, bond, err := id.SeatKeys(tags, sid)
	if err != nil {
		return membership.Credentials{}, err
	}
	return membership.Credentials{Session: session, Log: logKey, Bond: bond}, nil
}

// Seed reports a copy of the 32-byte seed, for derivations this package does not
// own (forfeit.PunishmentKeyFrom).
func (id *Identity) Seed() []byte {
	out := make([]byte, len(id.seed))
	copy(out, id.seed)
	return out
}

// Backup reports the secret everything this player owns is derived from.
func (id *Identity) Backup() (seedHex, bondOutpoint string) {
	id.mu.Lock()
	defer id.mu.Unlock()
	return hex.EncodeToString(id.seed), id.bondOutpoint
}

// Restore puts back a seed saved elsewhere, only onto a player that has never
// sat down. Swapping the seed under a live identity would strand its bond and
// change the keys its tables were agreed with.
func (id *Identity) Restore(seedHex, bondOutpoint string) error {
	seed, err := hex.DecodeString(strings.TrimSpace(seedHex))
	if err != nil || len(seed) != 32 {
		return fmt.Errorf("a seed is 32 bytes of hex")
	}
	outpoint := strings.TrimSpace(bondOutpoint)

	id.mu.Lock()
	defer id.mu.Unlock()
	if id.bondOutpoint != "" {
		return fmt.Errorf("this player already holds a bond, and restoring over it would strand that coin")
	}
	// Per-table-bond games never set the single bondOutpoint, so also refuse to
	// restore over any funded on-disk game state (recorded seat bonds, etc.).
	for _, mark := range stateMarks {
		if hasMark(id.dir, mark) {
			return fmt.Errorf("this directory already holds funded game state (%s); restoring a seed over it would strand that coin", mark)
		}
	}
	if err := id.writeLocked(seed, outpoint); err != nil {
		return err
	}
	id.seed, id.bondOutpoint = seed, outpoint
	return nil
}

// BondDeposit reports the one-per-identity bond outpoint, if any.
func (id *Identity) BondDeposit() string {
	id.mu.Lock()
	defer id.mu.Unlock()
	return id.bondOutpoint
}

// SetBondDeposit records the one-per-identity bond outpoint.
func (id *Identity) SetBondDeposit(outpoint string) error {
	id.mu.Lock()
	defer id.mu.Unlock()
	if err := id.writeLocked(id.seed, outpoint); err != nil {
		return err
	}
	id.bondOutpoint = outpoint
	return nil
}

// PayoutAddress reports where this player wants to be paid.
func (id *Identity) PayoutAddress() string {
	id.mu.Lock()
	defer id.mu.Unlock()
	return id.payout
}

// SetPayout records where this player wants to be paid, checked against the
// network first so a claim built to pay it cannot fail at the other seats.
func (id *Identity) SetPayout(addr string, params stdaddr.AddressParams) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return fmt.Errorf("no address")
	}
	if _, err := stdaddr.DecodeAddress(addr, params); err != nil {
		return fmt.Errorf("payout address: %w", err)
	}
	id.mu.Lock()
	defer id.mu.Unlock()
	id.payout = addr
	return id.writeLocked(id.seed, id.bondOutpoint)
}

// writeLocked replaces the identity file atomically.
func (id *Identity) writeLocked(seed []byte, outpoint string) error {
	out, err := json.Marshal(identityFile{
		SeedHex:      hex.EncodeToString(seed),
		BondOutpoint: outpoint,
		Payout:       id.payout,
	})
	if err != nil {
		return err
	}
	path := filepath.Join(id.dir, "identity.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("write identity: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write identity: %w", err)
	}
	return nil
}
