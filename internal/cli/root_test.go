package cli

import (
	"bytes"
	"context"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	defer func(v string) { Version = v }(Version)
	Version = "v1.2.3"
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"--version"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if got, want := stdout.String(), "bothways v1.2.3\n"; got != want {
		t.Fatalf("--version printed %q, want %q", got, want)
	}
}
