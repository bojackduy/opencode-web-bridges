//go:build darwin || linux

package chatgpt

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryTurnLock(f *os.File) error {
	return tryProfileLock(f, false)
}

func tryProfileLock(f *os.File, shared bool) error {
	mode := unix.LOCK_EX
	if shared {
		mode = unix.LOCK_SH
	}
	return unix.Flock(int(f.Fd()), mode|unix.LOCK_NB)
}

func turnLockContended(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}
