//go:build windows

package preview

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func shell(command string) (string, []string) { return "cmd", []string{"/C", command} }

func newProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// Windows has no signals: taskkill /T walks the process tree instead.
func terminateGroup(pid int) error { return taskkill(pid, false) }
func killGroup(pid int) error      { return taskkill(pid, true) }

func taskkill(pid int, force bool) error {
	args := []string{"/T", "/PID", strconv.Itoa(pid)}
	if force {
		args = append(args, "/F")
	}
	if err := exec.Command("taskkill", args...).Run(); err != nil && running(pid) {
		return err
	}
	return nil
}

func running(pid int) bool {
	id := strconv.Itoa(pid)
	out, err := exec.Command("tasklist", "/FI", "PID eq "+id, "/NH").Output()
	return err == nil && strings.Contains(string(out), id)
}

// processAge has no cheap equivalent here; Alive falls back to the pid alone.
func processAge(int) (time.Duration, bool) { return 0, false }
