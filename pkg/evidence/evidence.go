// Package evidence keeps both halves of an equivocation.
//
// The SDK's gamelog proves a seat signed two different things at one position and
// then discards the second message: the proof has been reached, and poker needs
// nothing more of it. Battleships needs more. The forfeitable bond is spent with
// the key those two signatures give up, and that spend can happen long after the
// match - across a restart, after a reorg, once the chain watcher finally sees
// the on-chain half - so the two signatures cannot live only in the moment they
// were noticed. This store retains them, persist-first, keyed by the position and
// the key that signed there.
//
// What recovers a key is always two wire signatures: their nonces derive from
// key and position alone, so a divergent pair shares r by construction and
// forfeit.Recover solves for the key. An on-chain signature can never be one of
// those halves - it is 65 bytes over a spending transaction, with no shared
// nonce - so a signature seen on chain triggers and evidences rather than
// recovers: it is retained, and it prompts a re-check of the wire halves already
// held. That seam is where the M6 chain watcher plugs in.
package evidence

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
)

// fileVersion is the on-disk format version, covered so a future layout cannot be
// read as this one.
const fileVersion = 1

const (
	digestLen = 32
	sigLen    = forfeit.SigLen // 64: an off-chain position-signed attestation
)

// Half is one retained signature: the digest that was signed and the signature
// over it. Two halves at one position with different digests are a proof.
type Half struct {
	Digest [32]byte
	Sig    []byte
}

// key identifies one position for one signer. The signer is part of the key
// because two seats sign their own boards at the same band position with
// different keys, and only two signatures by one key are equivocation.
type key struct {
	signer string
	match  string
	domain forfeit.Domain
	seq    uint64
}

// halfJSON is a retained signature in the on-disk form.
type halfJSON struct {
	Digest string `json:"digest"`
	Sig    string `json:"sig"`
}

// record is everything held at one position for one signer: the wire halves that
// can recover a key, and the chain halves that evidence and trigger.
type record struct {
	Signer string     `json:"signer"`
	Match  string     `json:"match"`
	Domain string     `json:"domain"`
	Seq    uint64     `json:"seq"`
	Wire   []halfJSON `json:"wire"`
	Chain  []halfJSON `json:"chain,omitempty"`
}

type fileJSON struct {
	Version int      `json:"version"`
	Records []record `json:"records"`
}

// Store is the persistent equivocation record. It survives a restart: recovery
// runs the same on the halves loaded from disk as on the halves seen live.
type Store struct {
	mu   sync.Mutex
	path string
	recs map[key]*record
}

// Open loads the store at path, or starts an empty one if the file does not yet
// exist.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("a store needs a path")
	}
	s := &Store{path: path, recs: map[key]*record{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f fileJSON
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("read evidence store: %w", err)
	}
	if f.Version != fileVersion {
		return nil, fmt.Errorf("evidence store is version %d, want %d", f.Version, fileVersion)
	}
	for i := range f.Records {
		r := f.Records[i]
		s.recs[key{signer: r.Signer, match: r.Match, domain: forfeit.Domain(r.Domain), seq: r.Seq}] = &r
	}
	return s, nil
}

// Record ingests a wire signature at a position.
//
// The first signature at a position is retained and nothing is exposed. A second
// signature identical to one already held is a no-op. A second signature that
// differs - the equivocation - is retained alongside the first, and the pair is
// solved: the cheat's key is returned. Both halves are kept, so the key can be
// re-derived after a restart.
func (s *Store) Record(signer *secp256k1.PublicKey, pos forfeit.Position, digest [32]byte, sig []byte) (*secp256k1.PrivateKey, error) {
	if signer == nil {
		return nil, fmt.Errorf("no signer")
	}
	if len(sig) != sigLen {
		return nil, fmt.Errorf("wire signature is %d bytes, want %d", len(sig), sigLen)
	}
	if pos.Match == "" || pos.Domain == "" {
		return nil, fmt.Errorf("a position needs a match and a domain")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	k := keyFor(signer, pos)
	rec := s.recs[k]
	incoming := halfJSON{Digest: hex.EncodeToString(digest[:]), Sig: hex.EncodeToString(sig)}

	if rec == nil {
		rec = newRecord(k)
		rec.Wire = append(rec.Wire, incoming)
		s.recs[k] = rec
		if err := s.persistLocked(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	for _, h := range rec.Wire {
		if h == incoming {
			return nil, nil // an honest re-issue, byte for byte
		}
	}
	rec.Wire = append(rec.Wire, incoming)
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return recoverFromWire(signer, rec.Wire)
}

// Trigger ingests a signature observed on chain at a position.
//
// An on-chain signature is never a Recover half: it is 65 bytes over a spending
// transaction and shares no nonce with the wire attestations. It is retained as
// evidence and it prompts a re-check - so if the store already holds a divergent
// pair of wire signatures at this position, that recovery is surfaced now. This
// is the seam the M6 chain watcher calls; recovery itself stays wire by wire.
func (s *Store) Trigger(signer *secp256k1.PublicKey, pos forfeit.Position, chainSig []byte) (*secp256k1.PrivateKey, error) {
	if signer == nil {
		return nil, fmt.Errorf("no signer")
	}
	if len(chainSig) == 0 {
		return nil, fmt.Errorf("no chain signature")
	}
	if pos.Match == "" || pos.Domain == "" {
		return nil, fmt.Errorf("a position needs a match and a domain")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	k := keyFor(signer, pos)
	rec := s.recs[k]
	if rec == nil {
		rec = newRecord(k)
		s.recs[k] = rec
	}
	seen := halfJSON{Sig: hex.EncodeToString(chainSig)}
	fresh := true
	for _, h := range rec.Chain {
		if h == seen {
			fresh = false
			break
		}
	}
	if fresh {
		rec.Chain = append(rec.Chain, seen)
	}
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return recoverFromWire(signer, rec.Wire)
}

// Retained returns copies of the wire halves held at a position, in the order
// they were seen, so a caller can assemble the proof for a dispute message.
func (s *Store) Retained(signer *secp256k1.PublicKey, pos forfeit.Position) []Half {
	if signer == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.recs[keyFor(signer, pos)]
	if rec == nil {
		return nil
	}
	out := make([]Half, 0, len(rec.Wire))
	for _, h := range rec.Wire {
		d, sig, err := h.bytes()
		if err != nil {
			continue
		}
		var dig [32]byte
		copy(dig[:], d)
		out = append(out, Half{Digest: dig, Sig: sig})
	}
	return out
}

// recoverFromWire returns the key from the first divergent pair of wire halves,
// or nil if no two of them prove an equivocation yet.
func recoverFromWire(signer *secp256k1.PublicKey, wire []halfJSON) (*secp256k1.PrivateKey, error) {
	for i := 0; i < len(wire); i++ {
		di, si, err := wire[i].bytes()
		if err != nil {
			return nil, err
		}
		for j := i + 1; j < len(wire); j++ {
			dj, sj, err := wire[j].bytes()
			if err != nil {
				return nil, err
			}
			if bytes.Equal(di, dj) {
				continue // the same message signed twice proves nothing
			}
			priv, err := forfeit.Recover(signer, di, si, dj, sj)
			if err != nil {
				continue // not a recovering pair; keep looking
			}
			return priv, nil
		}
	}
	return nil, nil
}

func newRecord(k key) *record {
	return &record{Signer: k.signer, Match: k.match, Domain: string(k.domain), Seq: k.seq}
}

func keyFor(signer *secp256k1.PublicKey, pos forfeit.Position) key {
	return key{
		signer: hex.EncodeToString(signer.SerializeCompressed()),
		match:  pos.Match,
		domain: pos.Domain,
		seq:    pos.Seq,
	}
}

func (h halfJSON) bytes() (digest, sig []byte, err error) {
	digest, err = hex.DecodeString(h.Digest)
	if err != nil {
		return nil, nil, fmt.Errorf("retained digest is not hex: %w", err)
	}
	sig, err = hex.DecodeString(h.Sig)
	if err != nil {
		return nil, nil, fmt.Errorf("retained signature is not hex: %w", err)
	}
	return digest, sig, nil
}

// persistLocked writes the store atomically: a temporary file replaced by rename,
// so a crash mid-write leaves the previous evidence intact rather than a partial
// file. The caller holds the mutex.
func (s *Store) persistLocked() error {
	f := fileJSON{Version: fileVersion}
	for _, r := range s.recs {
		f.Records = append(f.Records, *r)
	}
	sort.Slice(f.Records, func(i, j int) bool {
		a, b := f.Records[i], f.Records[j]
		if a.Signer != b.Signer {
			return a.Signer < b.Signer
		}
		if a.Match != b.Match {
			return a.Match < b.Match
		}
		if a.Domain != b.Domain {
			return a.Domain < b.Domain
		}
		return a.Seq < b.Seq
	})
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	// fsync the temp file and the directory around the rename: the retained
	// half spends the forfeitable bond, so it must survive power loss, not
	// just a torn write.
	tmp := s.path + ".tmp"
	f2, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f2.Write(data); err != nil {
		f2.Close()
		return err
	}
	if err := f2.Sync(); err != nil {
		f2.Close()
		return err
	}
	if err := f2.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
