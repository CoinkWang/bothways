//go:build !windows

package preview

import (
	"strings"
	"testing"
	"time"
)

func TestParseETime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"00:05":       5 * time.Second,
		"01:02:03":    time.Hour + 2*time.Minute + 3*time.Second,
		"2-00:00:01":  48*time.Hour + time.Second,
		" 10:00":      10 * time.Minute,
		"123-4:05:06": 123*24*time.Hour + 4*time.Hour + 5*time.Minute + 6*time.Second,
		"garbage":     -1,
		"1:2:3:4":     -1,
		"x-00:00:01":  -1,
		"00:-1":       -1,
	} {
		got, ok := parseETime(strings.TrimSpace(in))
		if want < 0 {
			if ok {
				t.Errorf("parseETime(%q) = %s, want failure", in, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("parseETime(%q) = %s, %v; want %s", in, got, ok, want)
		}
	}
}
