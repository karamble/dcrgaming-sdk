package spend

import "github.com/karamble/dcrgaming-sdk/internal/statelock"

// AcquireOwnership excludes another runtime using this file-backed book. Keep
// the returned lease until every worker has stopped. Memory books need no lease.
func (b *Book) AcquireOwnership() (func() error, error) {
	if s, ok := b.store.(*fileStore); ok {
		release, err := statelock.Acquire(s.path + ".lock")
		if err != nil {
			return nil, err
		}
		fresh, err := OpenBook(s)
		if err != nil {
			release()
			return nil, err
		}
		b.mu.Lock()
		b.records, b.pending, b.fault = fresh.records, fresh.pending, nil
		b.mu.Unlock()
		return release, nil
	}
	return func() error { return nil }, nil
}
