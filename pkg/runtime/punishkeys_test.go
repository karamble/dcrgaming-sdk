package runtime

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
	"github.com/karamble/dcrgaming-sdk/pkg/ruling"
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

	err := rt.Forfeit(context.Background(), ruling.Ruling{
		Match: sid, Against: against, Kind: ruling.Equivocation,
		Equivocated: equivocationAt(t),
	})
	if err == nil {
		t.Fatal("swept a bond nobody had funded")
	}
	if !strings.Contains(err.Error(), "not on the chain") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// The two kinds that still need an exchange say so, rather than guessing.
func TestTheRulingKindsThatNeedTheLadderSaySo(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	for _, rl := range []ruling.Ruling{
		{Match: "abcdef01", Kind: ruling.Silence, Silent: &ruling.Silent{Duty: "place", By: 900}},
		{Match: "abcdef01", Kind: ruling.Clean},
	} {
		if err := rt.Forfeit(context.Background(), rl); err == nil {
			t.Errorf("%s: carried out a stage that is not built", rl.Kind)
		}
	}
}

// An equivocation that exposes no key sweeps nothing, checked before any table
// is even looked at.
func TestARulingThatExposesNoKeySweepsNothing(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, _ := seatTwo(t, fake, rt)

	e := equivocationAt(t)
	e.SigB = append([]byte(nil), e.SigB...)
	e.SigB[0] ^= 0xff // no longer shares the nonce
	err := rt.Forfeit(context.Background(), ruling.Ruling{
		Match: sid, Against: 1, Kind: ruling.Equivocation, Equivocated: e,
	})
	if err == nil {
		t.Fatal("swept a bond on an equivocation that exposes nothing")
	}
	if !strings.Contains(err.Error(), "exposes no key") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// equivocationAt is a real pair of signatures at one position: two different
// statements, one nonce, so the key falls out.
func equivocationAt(t *testing.T) *ruling.Equivocated {
	t.Helper()
	k, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	at := forfeit.Position{Match: "m", Domain: forfeit.DomainEntry, Seq: 4}
	a := blake256.Sum256([]byte("seat 1 says left"))
	b := blake256.Sum256([]byte("seat 1 says right"))
	sigA, err := forfeit.Sign(k, at, a[:])
	if err != nil {
		t.Fatalf("sign a: %v", err)
	}
	sigB, err := forfeit.Sign(k, at, b[:])
	if err != nil {
		t.Fatalf("sign b: %v", err)
	}
	return &ruling.Equivocated{
		At: at, Pub: k.PubKey().SerializeCompressed(),
		HashA: a[:], SigA: sigA, HashB: b[:], SigB: sigB,
	}
}
