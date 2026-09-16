package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

type rejectTables struct{ *MemTableStore }

func (rejectTables) SaveTable(TableRecord) error { return errors.New("disk full") }
func TestAdmissionMustPersistBeforePayment(t *testing.T) {
	fake, rt := standAt(t, &trivialGame{}, t.TempDir(), NewMemTableStore())
	rt.store = rejectTables{NewMemTableStore()}
	if _, err := rt.AcceptInvite(context.Background(), invite(t, nil), testGCID); err == nil {
		t.Fatal("accepted without persistence")
	}
	if len(fake.Spends()) != 0 {
		t.Fatal("money requested without durable terms")
	}
	if rt.healthy() == nil {
		t.Fatal("storage fault not latched")
	}
}
func TestPendingAdmissionRestoresAndResyncs(t *testing.T) {
	fake, rt := standPerTable(t)
	fake.SetVerdict(bridgetest.Hold, "")
	sid, err := rt.AcceptInvite(context.Background(), invite(t, nil), testGCID)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pending spend", func() bool { return len(fake.Spends()) == 1 })
	records, err := rt.store.LoadTables()
	if err != nil || len(records) != 1 {
		t.Fatalf("pending table missing: %v", err)
	}
	rt.Resync(context.Background())
	if err = rt.Fund(context.Background(), sid); !errors.Is(err, ErrNotSeated) {
		t.Fatalf("pending fund: %v", err)
	}
	if !rt.HoldsOurs(sid) {
		t.Fatal("pending obligation can be dropped")
	}
	again, err := New(Config{Rules: rt.rules, Bridge: rt.bridge, Book: rt.book, Identity: rt.identity, Params: rt.params, Tables: rt.store, SeatTags: rt.seatTags})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	report, err := again.ResumeWithReport()
	if err != nil || len(report.Restored) != 1 {
		t.Fatalf("resume: %+v %v", report, err)
	}
	if again.Terms(sid) != rt.Terms(sid) {
		t.Fatal("terms lost")
	}
	snap, err := again.Snapshot(sid)
	if err != nil || snap.Phase != "admission" {
		t.Fatalf("snapshot: %+v %v", snap, err)
	}
	again.Resync(context.Background())
	if _, err = again.AcceptInvite(context.Background(), invite(t, nil), testGCID); err != nil {
		t.Fatal(err)
	}
	if _, err = again.AcceptInvite(context.Background(), invite(t, nil), "1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("conflicting gc accepted")
	}
	if len(fake.Spends()) != 1 {
		t.Fatal("duplicate payment")
	}
}

type resolvingGame struct{ trivialGame }

func (resolvingGame) ResolveInvite(_ context.Context, inv schema.Invite, terms membership.Terms) (membership.Terms, error) {
	terms.ForfeitBondAtoms = inv.BuyInAtoms
	return terms, nil
}
func TestInviteResolverDerivesEconomicsWithoutReplacingAdvertisedTerms(t *testing.T) {
	_, rt, _ := stand(t, &resolvingGame{})
	sid, err := rt.AcceptInvite(context.Background(), invite(t, nil), testGCID)
	if err != nil {
		t.Fatal(err)
	}
	terms := rt.Terms(sid)
	if terms.ForfeitBondAtoms != terms.BuyInAtoms {
		t.Fatal("invite economics not resolved")
	}
}
