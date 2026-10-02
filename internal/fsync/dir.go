//go:build !windows

// Package fsync makes a rename durable on every platform the SDK runs on.
package fsync

import "os"

// Dir flushes a directory so a rename into it survives a crash.
func Dir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
