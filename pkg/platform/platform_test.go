package platform

import (
	"testing"

	"github.com/Raccoon-CI/raccoonci/pkg/value"
)

func TestFromRunsOn(t *testing.T) {
	tests := []struct {
		runsOn any
		want   string
	}{
		{"ubuntu-latest", "linux"},
		{"Ubuntu-24.04", "linux"},
		{"macos-latest-large", "macos"},
		{"windows-2022", "windows"},
		{[]any{"self-hosted", "linux", "x64"}, "linux"},
		{[]any{"self-hosted", "macOS", "ARM64"}, "macos"},
		{value.Object{{Key: "group", Value: "big"}, {Key: "labels", Value: []any{"windows"}}}, "windows"},
		{value.Object{{Key: "group", Value: "big"}}, ""},
		{[]any{"self-hosted", "gpu"}, ""},
	}
	for _, tt := range tests {
		if got := FromRunsOn(nil, tt.runsOn); got != tt.want {
			t.Errorf("FromRunsOn(%v) = %q, want %q", tt.runsOn, got, tt.want)
		}
	}
}
