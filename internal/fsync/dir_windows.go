//go:build windows

// Package fsync makes a rename durable on every platform the SDK runs on.
package fsync

// Dir does nothing on Windows. A directory opens read-only there and flushing
// it is refused with "Access is denied", while NTFS journals the rename itself.
func Dir(string) error { return nil }
