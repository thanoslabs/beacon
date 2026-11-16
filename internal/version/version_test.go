package version

import (
	"strings"
	"testing"
)

func TestInfoString(t *testing.T) {
	tests := []struct {
		name string
		info Info
		want string
	}{
		{
			name: "release build",
			info: Info{Version: "1.2.3", Commit: "0123456789abcdef0123", GoVersion: "go1.27.4", Platform: "linux/amd64"},
			want: "beacon 1.2.3 (commit 0123456789ab, go1.27.4, linux/amd64)",
		},
		{
			name: "modified tree",
			info: Info{Version: "dev", Commit: "0123456789abcdef0123", Modified: true, GoVersion: "go1.27.4", Platform: "linux/amd64"},
			want: "beacon dev (commit 0123456789ab-modified, go1.27.4, linux/amd64)",
		},
		{
			name: "no vcs information",
			info: Info{Version: "dev", GoVersion: "go1.27.4", Platform: "darwin/arm64"},
			want: "beacon dev (commit unknown, go1.27.4, darwin/arm64)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.info.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetDefaults(t *testing.T) {
	info := Get()
	if info.Version != "dev" {
		t.Errorf("Version = %q, want %q for a build without ldflags", info.Version, "dev")
	}
	if !strings.HasPrefix(info.GoVersion, "go") {
		t.Errorf("GoVersion = %q, want a go version", info.GoVersion)
	}
}
