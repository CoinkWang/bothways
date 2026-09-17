//go:build windows

package cli

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	hookKernel32     = syscall.NewLazyDLL("kernel32.dll")
	procHookLockFile = hookKernel32.NewProc("LockFileEx")
	procHookUnlock   = hookKernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x1
	lockfileExclusive       = 0x2
	errorLockViolation      = syscall.Errno(33)
)

// detachSession is a no-op: there is no controlling terminal to escape.
func detachSession() {}

// tryLock takes an exclusive lock on path without waiting; ok is false when
// another process holds it.
func tryLock(path string) (unlock func(), ok bool, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	var ol syscall.Overlapped
	r, _, callErr := procHookLockFile.Call(f.Fd(), lockfileExclusive|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r == 0 {
		f.Close()
		if callErr == errorLockViolation {
			return nil, false, nil
		}
		return nil, false, callErr
	}
	return func() {
		var ol syscall.Overlapped
		procHookUnlock.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
		f.Close()
	}, true, nil
}
