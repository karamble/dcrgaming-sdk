package escrow

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/schnorr"
)

// bondPoPTag domain-separates a proof of possession, so a signature made to
// claim a bond can never be read as anything else that key signs.
var bondPoPTag = []byte("dcrpoker/bond/pop/v1")

// A bond is a public thing: it is coin on chain, under a script anybody can
// read, at an outpoint anybody can name. Pointing at one therefore proves
// nothing about who controls it, and without a proof of possession the first
// party to cite an outpoint is the one it counts for.
//
// That is not a hypothetical. It is how the referee behaves today - PostBond
// takes the owner key out of the script and files the bond under whoever
// presented it - and the only thing standing in the way is a first-come check
// on the outpoint, which makes claiming somebody else's bond a race rather than
// an impossibility. Peer to peer there is no shared registry to be first at, so
// every peer would have to accept every claim.
//
// The proof binds a holder as well as an outpoint, and that part is what stops
// it being copied. A join carries its proof in public, so a signature over the
// outpoint alone would be a token anybody could lift and present as their own.
// Naming the holder - the session key at a table, the player id at the referee -
// makes a stolen proof a proof about somebody else.

// BondPoPDigest is what a proof of possession signs.
//
// The layout is fixed and explicit rather than produced by an encoder. Two
// implementations have to agree byte for byte or the proof is worthless between
// them, and general encodings leave too much room to disagree.
func BondPoPDigest(outpoint string, holder []byte) ([32]byte, error) {
	outpoint = strings.TrimSpace(outpoint)
	if outpoint == "" {
		return [32]byte{}, fmt.Errorf("a bond proof must name an outpoint")
	}
	if len(holder) == 0 {
		return [32]byte{}, fmt.Errorf("a bond proof must name who is claiming it")
	}

	var b bytes.Buffer
	b.Write(bondPoPTag)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(outpoint)))
	b.WriteString(outpoint)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(holder)))
	b.Write(holder)
	return blake256.Sum256(b.Bytes()), nil
}

// SignBondPoP proves that whoever holds the bond's owner key is claiming it.
func SignBondPoP(outpoint string, holder []byte, priv *secp256k1.PrivateKey) ([]byte, error) {
	if priv == nil {
		return nil, fmt.Errorf("no signing key")
	}
	digest, err := BondPoPDigest(outpoint, holder)
	if err != nil {
		return nil, err
	}
	sig, err := schnorr.Sign(priv, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign bond proof: %w", err)
	}
	return sig.Serialize(), nil
}

// VerifyBondPoP checks a claim against the bond script it is made about.
//
// The owner key comes from the script rather than from the claimant, so this
// answers the only question worth asking: does the party claiming this bond
// control the key the bond pays out to.
func VerifyBondPoP(bond []byte, outpoint string, holder, sig []byte) error {
	terms, err := ParseBond(bond)
	if err != nil {
		return fmt.Errorf("bond script: %w", err)
	}
	digest, err := BondPoPDigest(outpoint, holder)
	if err != nil {
		return err
	}
	if len(sig) != schnorr.SignatureSize {
		return fmt.Errorf("bond proof is %d bytes, want %d", len(sig), schnorr.SignatureSize)
	}
	pub, err := secp256k1.ParsePubKey(terms.Owner)
	if err != nil {
		return fmt.Errorf("bond owner key: %w", err)
	}
	parsed, err := schnorr.ParseSignature(sig)
	if err != nil {
		return fmt.Errorf("parse bond proof: %w", err)
	}
	if !parsed.Verify(digest[:], pub) {
		return fmt.Errorf("bond proof does not verify against the bond's owner key")
	}
	return nil
}
