package openaicompat

import (
	"runtime/debug"
	"testing"
)

// TestUserAgentFromBuildInfo pins the version-resolution rule directly.
// defaultUserAgent cannot be steered from a test because debug.ReadBuildInfo
// reads the real binary, so the decision logic lives in a pure function that
// can be handed constructed build info.
//
// The rule that matters: when go-llm is imported, info.Main describes the
// CONSUMER, so reporting Main's version would misidentify this module.
func TestUserAgentFromBuildInfo(t *testing.T) {
	tests := []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{
			name: "no build info at all",
			info: nil,
			want: "go-llm/dev",
		},
		{
			name: "go-llm imported: dependency entry wins over the consumer's Main",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "github.com/kstruzzieri/firn-ide", Version: "v9.9.9"},
				Deps: []*debug.Module{
					{Path: "github.com/other/thing", Version: "v1.0.0"},
					{Path: modulePath, Version: "v0.2.0"},
				},
			},
			want: "go-llm/v0.2.0",
		},
		{
			name: "versioned replacement reports the compiled version",
			info: &debug.BuildInfo{Deps: []*debug.Module{{
				Path: modulePath, Version: "v0.2.0",
				Replace: &debug.Module{Path: modulePath, Version: "v0.3.0"},
			}}},
			want: "go-llm/v0.3.0",
		},
		{
			name: "local replacement with empty version reports dev",
			info: &debug.BuildInfo{Deps: []*debug.Module{{
				Path: modulePath, Version: "v0.2.0",
				Replace: &debug.Module{Path: "../go-llm"},
			}}},
			want: "go-llm/dev",
		},
		{
			name: "local replacement with devel version reports dev",
			info: &debug.BuildInfo{Deps: []*debug.Module{{
				Path: modulePath, Version: "v0.2.0",
				Replace: &debug.Module{Path: "../go-llm", Version: "(devel)"},
			}}},
			want: "go-llm/dev",
		},
		{
			name: "go-llm imported but absent from Deps: never report the consumer's version",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "github.com/kstruzzieri/firn-ide", Version: "v9.9.9"},
			},
			want: "go-llm/dev",
		},
		{
			name: "go-llm is the main module: Main's version is ours",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: modulePath, Version: "v0.3.1"},
			},
			want: "go-llm/v0.3.1",
		},
		{
			name: "plain go build stamps (devel), which must not reach the header",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: modulePath, Version: "(devel)"},
			},
			want: "go-llm/dev",
		},
		{
			name: "empty version is treated as unknown",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: modulePath},
				Deps: []*debug.Module{{Path: modulePath}},
			},
			want: "go-llm/dev",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := userAgentFromBuildInfo(tt.info); got != tt.want {
				t.Errorf("userAgentFromBuildInfo() = %q, want %q", got, tt.want)
			}
		})
	}
}
