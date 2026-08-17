package forfeit

import (
	"crypto/hmac"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// punishKeyTag domain-separates punishment keys from every other use of a
// seed. A new literal on purpose: never one of the frozen tag set.
var punishKeyTag = []byte("gaming/forfeit/punish/v1")

// PunishmentKeyFrom derives the key this player punishes one opponent with at
// one match.
//
// Deterministic where PunishmentKey is not, and that is the point: the bond a
// punishment key protects sits on chain for weeks, and a key drawn at random
// is gone after a restart while the bond it could take is still there. Binding
// the opponent's session key is what makes "one per opponent per match" - the
// property PunishmentKey's doc asks for but cannot enforce - hold by
// construction, so the key is rebuildable from nothing but the seed and the
// persisted roster.
//
// The derivation is HMAC-BLAKE256 keyed by the seed over the tag and the
// length-framed inputs, with the same counter walk coefficient uses to refuse
// a zero or overflowing scalar. The match is hashed exactly as given, so one
// that is not already trimmed is refused rather than normalized here - two
// peers that normalized differently would each be sure they were right.
func PunishmentKeyFrom(seed []byte, match string, opponentSession []byte) (*secp256k1.PrivateKey, error) {
	if len(seed) != 32 {
		return nil, fmt.Errorf("seed is %d bytes, want 32", len(seed))
	}
	if strings.TrimSpace(match) == "" {
		return nil, fmt.Errorf("a punishment key needs a match to belong to")
	}
	if match != strings.TrimSpace(match) {
		return nil, fmt.Errorf("a punishment key's match has surrounding space, and it is hashed exactly as given")
	}
	if len(opponentSession) != pubKeyLen {
		return nil, fmt.Errorf("opponent session key is %d bytes, want a %d byte compressed key",
			len(opponentSession), pubKeyLen)
	}
	if _, err := secp256k1.ParsePubKey(opponentSession); err != nil {
		return nil, fmt.Errorf("opponent session key: %w", err)
	}

	// The counter only ever advances past a zero or overflowing scalar,
	// which happens with probability around 2^-128.
	for i := uint32(0); i < 8; i++ {
		mac := hmac.New(blake256.New, seed)
		mac.Write(punishKeyTag)
		writeField(mac, []byte(match))
		writeField(mac, opponentSession)
		_ = binary.Write(mac, binary.BigEndian, i)

		var raw [32]byte
		copy(raw[:], mac.Sum(nil))
		var d secp256k1.ModNScalar
		if overflow := d.SetBytes(&raw); overflow == 0 && !d.IsZero() {
			return secp256k1.NewPrivateKey(&d), nil
		}
	}
	return nil, fmt.Errorf("could not derive a punishment key for match %s", match)
}
