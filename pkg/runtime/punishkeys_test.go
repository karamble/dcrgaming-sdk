package runtime

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/evidence"
	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

// theirPunishNote is the other seat announcing its punishment key, the way it
// would arrive over the wire.
func theirPunishNote(t *testing.T, rt *Runtime, sid string, them *peer) membership.PunishNote {
	t.Helper()
	rt.mu.Lock()
	tbl := rt.tables[sid]
	rt.mu.Unlock()

	theirSeat, ok := them.form.OurSeat()
	if !ok {
		t.Fatal("the other side has no seat")
	}
	seats, _ := them.form.Seats()
	matchID, ok := them.form.RosterHash()
	if !ok {
		t.Fatal("the other side has no roster")
	}
	var oppKey []byte
	for seat, k := range seats {
		if seat != theirSeat {
			oppKey = k
		}
	}
	punish, err := forfeit.PunishmentKeyFrom(them.seed.Seed(), hex.EncodeToString(matchID[:]), oppKey)
	if err != nil {
		t.Fatalf("their punishment key: %v", err)
	}
	note, err := membership.SignPunishNote(rt.punishTag, tbl.form.Terms(), matchID,
		theirSeat, them.creds.Session, punish)
	if err != nil {
		t.Fatalf("their announcement: %v", err)
	}
	return note
}

// Once both seats have announced, both forfeitable bonds exist - and not
// before, because a bond names the key that can take it.
func TestForfeitableBondsAppearOnceEverySeatHasAnnounced(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)

	// Our own announcement went out with seating.
	mine := ourSeatOf(t, rt, sid)
	if _, ok := rt.ForfeitableBond(sid, mine); ok {
		t.Fatal("a bond existed before both seats had announced")
	}
	if err := rt.adoptPunishKey(context.Background(), sid, theirPunishNote(t, rt, sid, them)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	for _, seat := range []uint32{0, 1} {
		b, ok := rt.ForfeitableBond(sid, seat)
		if !ok {
			t.Fatalf("seat %d has no forfeitable bond", seat)
		}
		if b.ScriptHex == "" || b.PkScriptHex == "" || b.Address == "" {
			t.Fatalf("seat %d's bond is incomplete: %+v", seat, b)
		}
	}
}

// Bonds are built only when every seat has announced. One seat's key alone
// names nothing that can be punished.
func TestBondsAreNotBuiltFromOneAnnouncement(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)

	// Forget our own announcement, as if it never got out.
	rt.mu.Lock()
	rt.tables[sid].punishPubs = map[uint32][]byte{}
	rt.mu.Unlock()

	if err := rt.adoptPunishKey(context.Background(), sid, theirPunishNote(t, rt, sid, them)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	for _, seat := range []uint32{0, 1} {
		if _, ok := rt.ForfeitableBond(sid, seat); ok {
			t.Fatalf("seat %d got a bond from one announcement", seat)
		}
	}
}

// An announcement that does not verify is not kept, and both signatures matter.
func TestAnAnnouncementThatDoesNotVerifyIsRefused(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	good := theirPunishNote(t, rt, sid, them)

	for _, tc := range []struct {
		name string
		mut  func(*membership.PunishNote)
	}{
		{"a forged possession signature", func(n *membership.PunishNote) { n.PopSig[0] ^= 0xff }},
		{"a forged session signature", func(n *membership.PunishNote) { n.SessionSig[0] ^= 0xff }},
		{"a key the announcer does not hold", func(n *membership.PunishNote) { n.Pub[1] ^= 0xff }},
		{"a seat that is not at the table", func(n *membership.PunishNote) { n.Seat = 9 }},
	} {
		bad := good
		bad.PopSig = append([]byte(nil), good.PopSig...)
		bad.SessionSig = append([]byte(nil), good.SessionSig...)
		bad.Pub = append([]byte(nil), good.Pub...)
		tc.mut(&bad)
		if err := rt.adoptPunishKey(context.Background(), sid, bad); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
	if _, ok := rt.ForfeitableBond(sid, 0); ok {
		t.Fatal("a bond was built from an announcement that did not verify")
	}
}

// A seat announcing twice with different keys is refused rather than
// reconciled: whichever bond was built first, the other announcement changes
// what can punish it.
func TestASeatCannotAnnounceTwoDifferentKeys(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	note := theirPunishNote(t, rt, sid, them)

	if err := rt.adoptPunishKey(context.Background(), sid, note); err != nil {
		t.Fatalf("first: %v", err)
	}
	// The same one again is fine - a repeat, not a change.
	if err := rt.adoptPunishKey(context.Background(), sid, note); err != nil {
		t.Fatalf("a repeated announcement was refused: %v", err)
	}
	// A second announcement that verifies perfectly well - a key the seat
	// really does hold - and is still refused, because a bond has been built
	// naming the first one.
	rt.mu.Lock()
	tbl := rt.tables[sid]
	rt.mu.Unlock()
	matchID, _ := them.form.RosterHash()
	theirSeat, _ := them.form.OurSeat()
	second, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("second key: %v", err)
	}
	other, err := membership.SignPunishNote(rt.punishTag, tbl.form.Terms(), matchID,
		theirSeat, them.creds.Session, second)
	if err != nil {
		t.Fatalf("their second announcement: %v", err)
	}
	if err := membership.VerifyPunishNote(rt.punishTag, tbl.form.Terms(), matchID, other,
		them.creds.Session.PubKey().SerializeCompressed()); err != nil {
		t.Fatalf("the second announcement does not verify, so this proves nothing: %v", err)
	}
	err = rt.adoptPunishKey(context.Background(), sid, other)
	if err == nil {
		t.Fatal("a seat announced a second, different key")
	}
	if !strings.Contains(err.Error(), "already announced") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// A sweep has to have somewhere it is allowed to pay, or nothing holds it to
// this operator.
func TestASweepNeedsAPinnedPayout(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	if err := rt.adoptPunishKey(context.Background(), sid, theirPunishNote(t, rt, sid, them)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	_, err := rt.pinnedPayout()
	if err == nil {
		t.Fatal("a punishment spend was allowed to go anywhere")
	}
	// Decoding an invented address would fail too; pin which refusal this is.
	if !strings.Contains(err.Error(), "no payout address has been set") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	if err := rt.setPayout(context.Background(), payTo(t)); err != nil {
		t.Fatalf("set payout: %v", err)
	}
	if _, err := rt.pinnedPayout(); err != nil {
		t.Fatalf("pinned payout: %v", err)
	}
}

// A bond nobody has funded is nothing to take.
func TestSweepingAnUnfundedBondIsRefused(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them := seatTwo(t, fake, rt)
	if err := rt.adoptPunishKey(context.Background(), sid, theirPunishNote(t, rt, sid, them)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if err := rt.setPayout(context.Background(), payTo(t)); err != nil {
		t.Fatalf("set payout: %v", err)
	}
	mine := ourSeatOf(t, rt, sid)
	against := uint32(1 - mine)

	err := rt.Seize(context.Background(), sid, against, exposedFor(t, them.creds.Log))
	if err == nil {
		t.Fatal("swept a bond nobody had funded")
	}
	if !strings.Contains(err.Error(), "not on the chain") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// The two verbs that still need an exchange say so at a table this peer is not
// at, rather than guessing.
func TestTheVerbsThatNeedTheLadderSaySo(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	ctx := context.Background()
	if err := rt.Accuse(ctx, "abcdef01", 1, Lapsed{Duty: "place", Seq: 1, By: 900}); err == nil {
		t.Error("accused at a table this peer is not at")
	}
	if err := rt.Release(ctx, "abcdef01"); err == nil {
		t.Error("released at a table this peer is not at")
	}
}

// exposedFor mints the evidence store's proof-of-exposure for a key, the way a
// game reaches one: two divergent signatures at one position, recorded.
func exposedFor(t *testing.T, priv *secp256k1.PrivateKey) *evidence.Exposed {
	t.Helper()
	pos := forfeit.Position{Match: "m", Domain: forfeit.DomainEntry, Seq: 4}
	digA := blake256.Sum256([]byte("seat 1 says left"))
	digB := blake256.Sum256([]byte("seat 1 says right"))
	sigA, err := forfeit.Sign(priv, pos, digA[:])
	if err != nil {
		t.Fatalf("sign a: %v", err)
	}
	sigB, err := forfeit.Sign(priv, pos, digB[:])
	if err != nil {
		t.Fatalf("sign b: %v", err)
	}
	store, err := evidence.Open(filepath.Join(t.TempDir(), "evidence.json"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.Record(priv.PubKey(), pos, digA, sigA); err != nil {
		t.Fatalf("first half: %v", err)
	}
	got, err := store.Record(priv.PubKey(), pos, digB, sigB)
	if err != nil || got == nil {
		t.Fatalf("a divergent pair recovered nothing: %v", err)
	}
	return got
}

// readyToSeize seats two, announces both punishment keys, pins a payout and
// puts the other seat's forfeitable bond on the chain: everything a seizure
// needs except the key.
func readyToSeize(t *testing.T, fake *bridgetest.Bridge, rt *Runtime) (string, *peer, uint32) {
	t.Helper()
	sid, them := seatTwo(t, fake, rt)
	if err := rt.adoptPunishKey(context.Background(), sid, theirPunishNote(t, rt, sid, them)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if err := rt.setPayout(context.Background(), payTo(t)); err != nil {
		t.Fatalf("set payout: %v", err)
	}
	against := uint32(1 - ourSeatOf(t, rt, sid))
	bond, ok := rt.ForfeitableBond(sid, against)
	if !ok {
		t.Fatalf("seat %d has no forfeitable bond", against)
	}
	script, err := hex.DecodeString(bond.PkScriptHex)
	if err != nil {
		t.Fatalf("pkScript: %v", err)
	}
	txid := strings.Repeat("f1", 32)
	fake.Place(txid, 0, script, int64(escrow.MinBondAtoms), fake.Height())
	rt.mu.Lock()
	tbl := rt.tables[sid]
	if tbl.forfeitFunded == nil {
		tbl.forfeitFunded = map[uint32]staked{}
	}
	tbl.forfeitFunded[against] = staked{outpoint: txid + ":0", atoms: int64(escrow.MinBondAtoms)}
	rt.mu.Unlock()
	return sid, them, against
}

// The happy path at the boundary a game sees: the accused's own key opens a
// branch of the bond this runtime derived, and the bond moves.
func TestSeizingWithTheSeatsOwnKeyTakesTheBond(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them, against := readyToSeize(t, fake, rt)

	if err := rt.Seize(context.Background(), sid, against, exposedFor(t, them.creds.Log)); err != nil {
		t.Fatalf("seize: %v", err)
	}
	rt.mu.Lock()
	outpoint := rt.tables[sid].forfeitFunded[against].outpoint
	rt.mu.Unlock()
	if !rt.isSweeping(outpoint) {
		t.Fatal("the bond was not swept")
	}
}

// The pin that licenses this runtime not re-deriving what escrow already
// refuses: a key that is not the accused's opens no branch, and nothing is
// built or sent.
func TestSeizingWithAKeyThatIsNotTheSeatsSeizesNothing(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _, against := readyToSeize(t, fake, rt)

	stranger, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	err = rt.Seize(context.Background(), sid, against, exposedFor(t, stranger))
	if err == nil {
		t.Fatal("swept a bond with a key that is not the accused's")
	}
	if !strings.Contains(err.Error(), "punishment branch") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	rt.mu.Lock()
	outpoint := rt.tables[sid].forfeitFunded[against].outpoint
	rt.mu.Unlock()
	if rt.isSweeping(outpoint) {
		t.Fatal("a refused seizure still marked the bond as being spent")
	}
}

// A seat does not seize from itself, and says so rather than failing four
// frames down as a bond with no branch for that key.
func TestASeatDoesNotSeizeItsOwnBond(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, them, _ := readyToSeize(t, fake, rt)

	mine := ourSeatOf(t, rt, sid)
	err := rt.Seize(context.Background(), sid, mine, exposedFor(t, them.creds.Log))
	if err == nil {
		t.Fatal("seized this seat's own bond")
	}
	if !strings.Contains(err.Error(), "not seized from oneself") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// A seizure with nothing exposed is refused before any table is looked at.
func TestSeizingWithNoKeyTakesNothing(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	if err := rt.Seize(context.Background(), "abcdef01", 1, nil); err == nil {
		t.Fatal("seized on no key at all")
	}
}
