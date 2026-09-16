package spend

import (
	"errors"
	"sync"
	"testing"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
)

type brokenStore struct {
	records []Record
	fail    bool
}

func (s *brokenStore) Load() ([]Record, error) { return append([]Record(nil), s.records...), nil }
func (s *brokenStore) Save(rs []Record) error {
	if s.fail {
		return errors.New("disk full")
	}
	s.records = append([]Record(nil), rs...)
	return nil
}
func obligation() Record {
	return Record{Match: "table", Purpose: "stake", Address: "Ts", Atoms: 123, PkScript: "aa", Obligation: "terms-and-key"}
}
func TestConcurrentReservationAuthorizesOnlyOneDispatch(t *testing.T) {
	b, err := OpenBook(MemStore())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	fresh := make(chan bool, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, yes, err := b.Reserve(obligation())
			if err != nil {
				t.Error(err)
			}
			fresh <- yes
		}()
	}
	wg.Wait()
	close(fresh)
	count := 0
	for yes := range fresh {
		if yes {
			count++
		}
	}
	if count != 1 || len(b.All()) != 1 {
		t.Fatalf("dispatches=%d records=%d", count, len(b.All()))
	}
	reopened, err := OpenBook(b.store)
	if err != nil {
		t.Fatal(err)
	}
	if _, yes, err := reopened.Reserve(obligation()); err != nil || yes {
		t.Fatalf("restart dispatched again: %v %v", yes, err)
	}
	changed := obligation()
	changed.Atoms++
	if _, _, err := reopened.Reserve(changed); err == nil {
		t.Fatal("changed terms accepted")
	}
}
func TestFailedSaveDoesNotCommitMemoryOrPermitLaterWrites(t *testing.T) {
	s := &brokenStore{}
	b, _ := OpenBook(s)
	if _, _, err := b.Reserve(obligation()); err != nil {
		t.Fatal(err)
	}
	s.fail = true
	if _, err := b.Adopt("table", "stake", "request"); err == nil {
		t.Fatal("write succeeded")
	}
	if _, ok := b.Get("request"); ok {
		t.Fatal("failed adoption changed memory")
	}
	if b.All()[0].ID != "" {
		t.Fatal("pending intent lost")
	}
	s.fail = false
	if _, err := b.Adopt("table", "stake", "request"); err == nil {
		t.Fatal("fault was not latched")
	}
	reopened, _ := OpenBook(s)
	if _, err := reopened.Adopt("table", "stake", "request"); err != nil {
		t.Fatal(err)
	}
}
func TestExplicitRetryPreservesRefusedAttempt(t *testing.T) {
	b, _ := OpenBook(MemStore())
	b.Reserve(obligation())
	b.Adopt("table", "stake", "first")
	if _, err := b.Retry("first"); err == nil {
		t.Fatal("pending retried")
	}
	b.Note("first", transport.Spend{State: transport.SpendDenied}, nil)
	next, err := b.Retry("first")
	if err != nil {
		t.Fatal(err)
	}
	if next.Attempt != 1 || next.ID != "" || len(b.All()) != 2 {
		t.Fatal("attempt history lost")
	}
	if _, err := b.Retry("first"); err == nil {
		t.Fatal("same refusal retried twice")
	}
	if _, fresh, err := b.Reserve(obligation()); err != nil || fresh {
		t.Fatal("unresolved retry redispatched", err)
	}
}
