//go:build !windows

package preview

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func shell(command string) (string, []string) { return "sh", []string{"-c", command} }

// newProcessGroup makes the child a process-group leader so that signals reach
// everything the shell spawned, not just the shell.
func newProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateGroup(pid int) error { return signalGroup(pid, syscall.SIGTERM) }
func killGroup(pid int) error      { return signalGroup(pid, syscall.SIGKILL) }

func signalGroup(pid int, sig syscall.Signal) error {
	err := syscall.Kill(-pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		// No group with that id: the child may never have become a leader.
		err = syscall.Kill(pid, sig)
	}
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func running(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// processAge asks ps(1) how long pid has been running. Its elapsed-time format
// is POSIX; anything unexpected reports ok=false and the caller falls back to
// the pid alone.
func processAge(pid int) (time.Duration, bool) {
	out, err := exec.Command("ps", "-o", "etime=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	return parseETime(strings.TrimSpace(string(out)))
}

// parseETime reads ps's [[DD-]HH:]MM:SS elapsed time.
func parseETime(s string) (time.Duration, bool) {
	days := 0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, false
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	secs := 0
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return 0, false
		}
		secs = secs*60 + n
	}
	return time.Duration(days)*24*time.Hour + time.Duration(secs)*time.Second, true
}
