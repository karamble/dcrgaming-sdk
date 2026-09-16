package runtime

import (
	"fmt"
	"github.com/karamble/dcrgaming-sdk/internal/statelock"
	"path/filepath"
)

func (r *Runtime) acquireOwnership() error {
	release, err := r.book.AcquireOwnership()
	if err != nil {
		return err
	}
	r.releases = append(r.releases, release)
	if f, ok := r.store.(*FileTableStore); ok {
		release, err = statelock.Acquire(filepath.Join(f.dir, ".runtime.lock"))
		if err != nil {
			r.releaseOwnership()
			return err
		}
		r.releases = append(r.releases, release)
	}
	return nil
}
func (r *Runtime) releaseOwnership() {
	for _, release := range r.releases {
		_ = release()
	}
	r.releases = nil
}

// Close releases ownership for a runtime never started with Run. A running
// runtime must be canceled and awaited; Run releases leases after workers stop.
func (r *Runtime) Close() error {
	r.lifeMu.Lock()
	defer r.lifeMu.Unlock()
	if r.running {
		return fmt.Errorf("cancel and await Run before closing runtime")
	}
	r.stopping = true
	r.releaseOwnership()
	return nil
}
