package preview

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLastURL(t *testing.T) {
	for _, tc := range []struct {
		out, want string
		ok        bool
	}{
		{"building\nhttps://shape-circle.example.test\n", "https://shape-circle.example.test", true},
		{"https://a.test/x\n\n  \n", "https://a.test/x", true},
		{"http://localhost:3001\r\n", "http://localhost:3001", true},
		{"https://a.test\ndone\n", "", false},
		{"ftp://a.test\n", "", false},
		{"/just/a/path\n", "", false},
		{"", "", false},
	} {
		got, err := LastURL(tc.out)
		if tc.ok != (err == nil) || got != tc.want {
			t.Errorf("LastURL(%q) = %q, %v; want %q (ok=%v)", tc.out, got, err, tc.want, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrNoURL) {
			t.Errorf("LastURL(%q) error %v is not ErrNoURL", tc.out, err)
		}
	}
}

func TestRenderEnv(t *testing.T) {
	v := Vars{Slot: 2, Port: 3002, Name: "shape-circle", Decision: "shape", Variant: "circle", Worktree: "/wt"}
	env, err := RenderEnv(map[string]string{"PORT": "{{.Port}}", "DB": "app_{{.Slot}}_{{.Variant}}"}, v)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"DB=app_2_circle", "PORT=3002", "BOTHWAYS_SLOT=2", "BOTHWAYS_NAME=shape-circle",
		"BOTHWAYS_WORKTREE=/wt", "BOTHWAYS_DECISION=shape", "BOTHWAYS_VARIANT=circle"}
	if strings.Join(env, " ") != strings.Join(want, " ") {
		t.Errorf("RenderEnv = %v, want %v", env, want)
	}
	if _, err := Render("start", "serve {{.Nope}}", v); err == nil {
		t.Error("unknown template variable should fail")
	}
}

func TestHealthURL(t *testing.T) {
	for _, tc := range [][3]string{
		{"http://localhost:3001", "/", "http://localhost:3001/"},
		{"http://localhost:3001/", "/healthz", "http://localhost:3001/healthz"},
		{"http://localhost:3001", "ready", "http://localhost:3001/ready"},
		{"http://localhost:3001", "", "http://localhost:3001"},
	} {
		if got := HealthURL(tc[0], tc[1]); got != tc[2] {
			t.Errorf("HealthURL(%q, %q) = %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
}

func TestDeployTimeoutKillsTheScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	dir := t.TempDir()
	start := time.Now()
	_, err := Deploy(context.Background(), Spec{Dir: dir, Command: "sleep 30; echo https://late.test", Log: filepath.Join(dir, "log")}, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Deploy = %v, want a timeout", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Deploy took %s after its timeout", d)
	}
}

// TestStartHealthStop is the start-mode lifecycle against a real HTTP server.
func TestStartHealthStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	dir := t.TempDir()
	port := freePort(t)
	pidFile := filepath.Join(dir, "p.pid")
	proc, err := Start(Spec{
		Dir:     dir,
		Command: "exec python3 -m http.server --bind 127.0.0.1 " + strconv.Itoa(port),
		Log:     filepath.Join(dir, "p.log"),
	}, pidFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Stop(proc.PID, time.Second) })

	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/"
	if err := WaitHealthy(context.Background(), url, 20*time.Second, 100*time.Millisecond, proc.Exited()); err != nil {
		log, _ := os.ReadFile(filepath.Join(dir, "p.log"))
		t.Fatalf("WaitHealthy: %v\n%s", err, log)
	}
	if data, _ := os.ReadFile(pidFile); strings.TrimSpace(string(data)) != strconv.Itoa(proc.PID) {
		t.Errorf("pid file holds %q, want %d", data, proc.PID)
	}
	if !Alive(proc.PID, proc.StartedAt) {
		t.Fatal("a running preview reads as dead")
	}
	// A start time long before the process existed means the pid was recycled.
	if Alive(proc.PID, proc.StartedAt.Add(-time.Hour)) {
		t.Error("a pid younger than its recorded start time reads as alive")
	}

	if err := Stop(proc.PID, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-proc.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("process still running after Stop")
	}
	if Alive(proc.PID, proc.StartedAt) {
		t.Error("a stopped preview reads as alive")
	}
}

func TestWaitHealthyNoticesExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	dir := t.TempDir()
	proc, err := Start(Spec{Dir: dir, Command: "exit 3"}, "")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = WaitHealthy(context.Background(), "http://127.0.0.1:"+strconv.Itoa(freePort(t))+"/", time.Minute, 50*time.Millisecond, proc.Exited())
	if !errors.Is(err, ErrExited) {
		t.Fatalf("WaitHealthy = %v, want ErrExited", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("WaitHealthy kept polling a process that had exited")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
