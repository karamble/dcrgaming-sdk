package fsync

import (
	"os"
	"path/filepath"
	"testing"
)

// A rename followed by Dir must succeed on every platform; on Windows a raw
// directory flush is refused, which once broke every table the runtime saved.
func TestDirAfterRename(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "a.tmp")
	if err := os.WriteFile(tmp, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "a")); err != nil {
		t.Fatal(err)
	}
	if err := Dir(dir); err != nil {
		t.Fatal(err)
	}
}
