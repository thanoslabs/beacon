package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// runCLI runs the command line with an empty environment and returns the exit
// code and what was written to stdout and stderr.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(context.Background(), args, func(string) string { return "" }, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no arguments prints usage", args: nil, wantCode: exitUsage, wantStderr: "Usage:"},
		{name: "help", args: []string{"help"}, wantCode: exitOK, wantStdout: "Commands:"},
		{name: "help flag", args: []string{"--help"}, wantCode: exitOK, wantStdout: "Commands:"},
		{name: "unknown command", args: []string{"nope"}, wantCode: exitUsage, wantStderr: `unknown command "nope"`},
		{name: "version", args: []string{"version"}, wantCode: exitOK, wantStdout: "beacon dev"},
		{name: "version flag", args: []string{"--version"}, wantCode: exitOK, wantStdout: "beacon dev"},
		{name: "version help", args: []string{"version", "-h"}, wantCode: exitOK, wantStderr: "beacon version"},
		{name: "version rejects arguments", args: []string{"version", "extra"}, wantCode: exitUsage, wantStderr: `unexpected argument "extra"`},
		{name: "version rejects unknown flags", args: []string{"version", "--nope"}, wantCode: exitUsage, wantStderr: "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if !strings.Contains(stdout, tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout, tt.wantStdout)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
		})
	}
}

func TestUsageListsEveryCommand(t *testing.T) {
	_, stdout, _ := runCLI(t, "help")
	for name := range commands() {
		if !strings.Contains(stdout, "  "+name+" ") {
			t.Errorf("usage does not list command %q:\n%s", name, stdout)
		}
	}
}
