//go:build windows

package chatgpt

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func tryTurnLock(f *os.File) error {
	return tryProfileLock(f, false)
}

func tryProfileLock(f *os.File, shared bool) error {
	// Lock the same first byte for every holder. Windows releases the lock
	// on handle close or process exit, just like flock on Unix.
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if !shared {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	return windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &windows.Overlapped{})
}

func turnLockContended(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
