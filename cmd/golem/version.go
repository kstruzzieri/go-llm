package main

import "runtime/debug"

// Build metadata. GoReleaser overrides these via -ldflags -X at release time
// (see .goreleaser.yaml). For `go install ...@vX.Y.Z` builds they stay empty
// and versionString falls back to the module version the Go toolchain stamps
// into the build info, so both distribution paths report a real version.
var (
	version = ""
	commit  = ""
	date    = ""
)

// versionString renders the golem build identity printed by -version.
func versionString() string {
	v := version
	if v == "" {
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
			v = info.Main.Version
		}
	}
	if v == "" {
		v = "dev"
	}
	out := "golem " + v
	if commit != "" {
		out += " (" + commit + ")"
	}
	if date != "" {
		out += " built " + date
	}
	return out
}
