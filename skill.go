// Package bothways carries the files the binary ships with.
package bothways

import _ "embed"

// Skill is the agent skill, so that `bothways skill` hands out the one written
// for this build rather than whatever the repository holds today.
//
//go:embed plugins/bothways/skills/bothways/SKILL.md
var Skill string
