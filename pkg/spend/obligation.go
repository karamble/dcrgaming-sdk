package spend

import (
	"fmt"
	"strings"
)

// Reserve atomically records one dispatch intent. A pre-existing obligation is
// returned without permission to dispatch, even when its bridge id is unknown.
func (b *Book) Reserve(want Record) (Record, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if want.Match == "" || want.Purpose == "" || want.Address == "" || want.Atoms <= 0 || want.Obligation == "" {
		return Record{}, false, fmt.Errorf("incomplete obligation")
	}
	var found *Record
	for _, old := range b.allLocked() {
		if old.Match != want.Match || old.Purpose != want.Purpose || old.Seat != want.Seat {
			continue
		}
		if old.Address != want.Address || old.Atoms != want.Atoms || !strings.EqualFold(old.PkScript, want.PkScript) || (old.Obligation != "" && old.Obligation != want.Obligation) {
			return Record{}, false, fmt.Errorf("conflicting obligation terms")
		}
		if found != nil && old.Attempt == found.Attempt {
			return Record{}, false, fmt.Errorf("ambiguous obligation requires reconciliation")
		}
		if found == nil || old.Attempt > found.Attempt {
			copy := old
			found = &copy
		}
	}
	if found != nil {
		return *found, false, nil
	}
	want.State = Requested
	if err := b.commitLocked(want); err != nil {
		return Record{}, false, err
	}
	return want, true, nil
}

// Retry reserves an explicitly authorized attempt only after authoritative
// refusal/expiry. Failed and unknown payments are deliberately not retryable.
func (b *Book) Retry(id string) (Record, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	old, ok := b.records[id]
	if !ok {
		return Record{}, fmt.Errorf("unknown request")
	}
	if (old.State != Denied && old.State != Expired) || old.TxID != "" || old.Outpoint != "" {
		return Record{}, fmt.Errorf("payment is not proven unpaid")
	}
	for _, r := range b.allLocked() {
		if r.Match == old.Match && r.Seat == old.Seat && r.Purpose == old.Purpose && r.Attempt > old.Attempt {
			return Record{}, fmt.Errorf("a later attempt already exists")
		}
	}
	next := old
	next.ID = ""
	next.Attempt++
	next.State = Requested
	next.Error = ""
	next.Unreachable = ""
	return next, b.commitLocked(next)
}
