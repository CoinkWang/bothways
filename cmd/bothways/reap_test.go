package main

import (
	"io/fs"
	"path/filepath"
	"time"

	"github.com/CoinkWang/bothways/internal/preview"
	"github.com/CoinkWang/bothways/internal/state"
)

// reapPreviews stops every preview a script left running. Previews outlive
// the bothways process that started them by design, so a script that fails
// before its `bothways down` would otherwise leak a server. Only processes
// whose pid and start time match a recorded preview are touched.
func reapPreviews(workDir string) func() {
	return func() {
		filepath.WalkDir(workDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() || d.Name() != "bothways" {
				return nil
			}
			st, err := (&state.Store{Dir: path}).Load()
			if err != nil {
				return nil
			}
			for _, dec := range st.Decisions {
				for _, v := range dec.Variants {
					if p := v.Preview; p != nil && preview.Alive(p.PID, p.StartedAt) {
						preview.Stop(p.PID, 2*time.Second)
					}
				}
			}
			return nil
		})
	}
}
