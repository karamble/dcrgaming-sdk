package membership

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"
)

// PunishNote is one seat announcing the key its opponent's forfeitable bond
// will name.
//
// It carries two signatures over the same digest and needs both. The session
// signature says who is announcing, so nobody can announce on another seat's
// behalf. The proof-of-possession signature says the announced key is one the
// announcer actually holds, so nobody can name a key they cannot use - a seat
// that did that would build its opponent a bond nobody could ever punish, which
// is a bond that is not a bond.
//
// An announcement without a verifying proof of possession is not an
// announcement.
type PunishNote struct {
	Seat uint32 `json:"seat"`
	// Pub is the announced punishment key, compressed.
	Pub []byte `json:"pub"`
	// SessionSig is the announcer's session key over the digest, and PopSig
	// is the announced key over the same digest.
	SessionSig []byte `json:"sessionSig"`
	PopSig     []byte `json:"popSig"`
}

// PunishDigest is what both signatures cover.
//
// The terms, the settled roster, the announcing seat and the key itself, so an
// announcement cannot be lifted to another table, another roster, another seat
// or another key.
//
// The domain tag is a parameter and not a constant here. It is a frozen hash
// input - changing it invalidates every announcement made under the old one -
// and which value a game froze is the game's own history, not something an SDK
// may decide on its behalf. dcrbattleships uses "battleships/punishkey/v1".
func PunishDigest(tag []byte, t Terms, roster [32]byte, seat uint32, pub []byte) ([32]byte, error) {
	if len(tag) == 0 {
		return [32]byte{}, fmt.Errorf("a punishment-key announcement needs a domain tag")
	}
	th, err := t.Hash()
	if err != nil {
		return [32]byte{}, err
	}
	var b bytes.Buffer
	b.Write(tag)
	b.Write(th[:])
	b.Write(roster[:])
	_ = binary.Write(&b, binary.BigEndian, seat)
	b.Write(pub)
	return blake256.Sum256(b.Bytes()), nil
}

// SignPunishNote builds this seat's announcement.
func SignPunishNote(tag []byte, t Terms, roster [32]byte, seat uint32,
	session, punish *secp256k1.PrivateKey) (PunishNote, error) {

	if session == nil || punish == nil {
		return PunishNote{}, fmt.Errorf("announcing a punishment key needs both keys")
	}
	pub := punish.PubKey().SerializeCompressed()
	digest, err := PunishDigest(tag, t, roster, seat, pub)
	if err != nil {
		return PunishNote{}, err
	}
	auth, err := schnorr.Sign(session, digest[:])
	if err != nil {
		return PunishNote{}, err
	}
	pop, err := schnorr.Sign(punish, digest[:])
	if err != nil {
		return PunishNote{}, err
	}
	return PunishNote{
		Seat: seat, Pub: pub,
		SessionSig: auth.Serialize(), PopSig: pop.Serialize(),
	}, nil
}

// VerifyPunishNote checks an announcement against the announcing seat's session
// key and against the announced key itself.
func VerifyPunishNote(tag []byte, t Terms, roster [32]byte, n PunishNote, sessionPub []byte) error {
	if len(n.Pub) != 33 {
		return fmt.Errorf("an announced punishment key is %d bytes, want %d", len(n.Pub), 33)
	}
	digest, err := PunishDigest(tag, t, roster, n.Seat, n.Pub)
	if err != nil {
		return err
	}
	check := func(pub, sig []byte, what string) error {
		pk, err := secp256k1.ParsePubKey(pub)
		if err != nil {
			return fmt.Errorf("%s key: %w", what, err)
		}
		s, err := schnorr.ParseSignature(sig)
		if err != nil {
			return fmt.Errorf("%s signature: %w", what, err)
		}
		if !s.Verify(digest[:], pk) {
			return fmt.Errorf("the %s signature does not verify", what)
		}
		return nil
	}
	if err := check(sessionPub, n.SessionSig, "session"); err != nil {
		return err
	}
	return check(n.Pub, n.PopSig, "possession")
}
