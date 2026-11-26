package config

import "strings"

// UnpublishedHooksImage is the hooks image several wrapped modules name; it is not built or
// published yet (deploy/hooks). Remove this once a release publishes it.
const UnpublishedHooksImage = "ghcr.io/dodwmd/can-hooks:"

// BackupHookUnrunnable says why the module's backup hook cannot run as shipped, or "".
func (m *Module) BackupHookUnrunnable() string {
	switch {
	case m.Hooks.Backup == "", m.Hooks.Command != "", m.Hooks.Builtin != nil:
		return "" // a subcommand of the module's own image, or can-core's own dump
	case m.Hooks.Image == "":
		return "it names no hooks.image to run in"
	case strings.HasPrefix(m.Hooks.Image, UnpublishedHooksImage):
		return "it needs the can-hooks image, which is not published yet"
	}
	return ""
}
