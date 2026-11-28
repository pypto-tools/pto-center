// Package buildversion identifies the running Center or Agent executable.
package buildversion

import (
	"runtime/debug"

	p "github.com/pypto-tools/pto-center/protocol/v1"
)

// Release is set to the immutable package version by the release build.
var Release string

func Current() string {
	if Release != "" {
		return Release
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if p.Version.MatchString(info.Main.Version) {
		return info.Main.Version
	}
	var revision, modified string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if len(setting.Value) >= 12 {
				revision = setting.Value[:12]
			}
		case "vcs.modified":
			if setting.Value == "true" {
				modified = ".dirty"
			}
		}
	}
	if revision != "" {
		return "dev-" + revision + modified
	}
	return "dev"
}
