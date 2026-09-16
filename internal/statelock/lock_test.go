package statelock

import (
	"path/filepath"
	"testing"
)

func TestOnlyOneStateOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	release, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if another, err := Acquire(path); err == nil {
		another()
		release()
		t.Fatal("two owners")
	}
	if err = release(); err != nil {
		t.Fatal(err)
	}
	next, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer next()
}
