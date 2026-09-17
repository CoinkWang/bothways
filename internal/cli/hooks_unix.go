//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// detachSession moves the hook's background sync into its own session, so a
// Ctrl-C or a closing terminal aimed at the committer cannot stop a rebase
// halfway.
func detachSession() {
	syscall.Setsid()
}

// tryLock takes an exclusive lock on path without waiting; ok is false when
// another process holds it.
func tryLock(path string) (unlock func(), ok bool, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, true, nil
}
