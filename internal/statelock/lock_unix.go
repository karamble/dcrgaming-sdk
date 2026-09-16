//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package statelock

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

func Acquire(path string) (func() error, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("state already in use (%s): %w", path, err)
	}
	return f.Close, nil
}
