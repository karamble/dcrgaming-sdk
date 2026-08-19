package membership

import (
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

var testPunishTag = []byte("testgame/punishkey/v1")

func punishKeys(t *testing.T) (session, punish *secp256k1.PrivateKey) {
	t.Helper()
	var err error
	if session, err = secp256k1.GeneratePrivateKey(); err != nil {
		t.Fatalf("session: %v", err)
	}
	if punish, err = secp256k1.GeneratePrivateKey(); err != nil {
		t.Fatalf("punish: %v", err)
	}
	return session, punish
}

func punishTerms() Terms {
	return Terms{
		Game: "testgame", GameVer: 1, SID: "abcdef01",
		BuyInAtoms: 5_000_000, Seats: 2, CSVBlocks: 2048, Until: 900,
	}
}

func TestAnAnnouncementVerifiesForItsOwnSeat(t *testing.T) {
	session, punish := punishKeys(t)
	tm, roster := punishTerms(), [32]byte{1, 2, 3}

	n, err := SignPunishNote(testPunishTag, tm, roster, 1, session, punish)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := VerifyPunishNote(testPunishTag, tm, roster, n, session.PubKey().SerializeCompressed()); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// Both signatures are required and each catches a different lie: the session
// one that somebody else is announcing, the possession one that the announced
// key is not held by the announcer.
func TestAnAnnouncementNeedsBothSignatures(t *testing.T) {
	session, punish := punishKeys(t)
	tm, roster := punishTerms(), [32]byte{1, 2, 3}
	good, err := SignPunishNote(testPunishTag, tm, roster, 1, session, punish)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	sessionPub := session.PubKey().SerializeCompressed()

	// A key the announcer does not hold: the possession signature cannot be
	// made, so borrowing somebody else's is the only option, and it fails.
	_, other := punishKeys(t)
	borrowed := good
	borrowed.Pub = other.PubKey().SerializeCompressed()
	if err := VerifyPunishNote(testPunishTag, tm, roster, borrowed, sessionPub); err == nil {
		t.Error("accepted a key the announcer cannot use; the bond would be unpunishable")
	}

	// Announced by somebody who is not that seat.
	otherSession, _ := punishKeys(t)
	if err := VerifyPunishNote(testPunishTag, tm, roster, good,
		otherSession.PubKey().SerializeCompressed()); err == nil {
		t.Error("accepted an announcement made on another seat's behalf")
	}

	for _, tc := range []struct {
		name string
		mut  func(*PunishNote)
	}{
		{"no session signature", func(n *PunishNote) { n.SessionSig = nil }},
		{"no possession signature", func(n *PunishNote) { n.PopSig = nil }},
		{"a truncated key", func(n *PunishNote) { n.Pub = n.Pub[:32] }},
	} {
		bad := good
		tc.mut(&bad)
		if err := VerifyPunishNote(testPunishTag, tm, roster, bad, sessionPub); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

// The digest binds the announcement to one table, one roster, one seat and one
// key, so none of it can be lifted somewhere else.
func TestAnAnnouncementCannotBeLiftedElsewhere(t *testing.T) {
	session, punish := punishKeys(t)
	tm, roster := punishTerms(), [32]byte{1, 2, 3}
	good, err := SignPunishNote(testPunishTag, tm, roster, 1, session, punish)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	sessionPub := session.PubKey().SerializeCompressed()

	otherTerms := punishTerms()
	otherTerms.SID = "beefcafe"
	for _, tc := range []struct {
		name   string
		tag    []byte
		terms  Terms
		roster [32]byte
		note   PunishNote
	}{
		{"another table", testPunishTag, otherTerms, roster, good},
		{"another roster", testPunishTag, tm, [32]byte{9}, good},
		{"another domain tag", []byte("othergame/punishkey/v1"), tm, roster, good},
		{"another seat", testPunishTag, tm, roster, PunishNote{
			Seat: 0, Pub: good.Pub, SessionSig: good.SessionSig, PopSig: good.PopSig,
		}},
	} {
		if err := VerifyPunishNote(tc.tag, tc.terms, tc.roster, tc.note, sessionPub); err == nil {
			t.Errorf("%s: an announcement was lifted to it", tc.name)
		}
	}
}

func TestADigestNeedsADomainTag(t *testing.T) {
	if _, err := PunishDigest(nil, punishTerms(), [32]byte{}, 0, make([]byte, 33)); err == nil {
		t.Fatal("hashed an announcement with no domain separation")
	}
}
