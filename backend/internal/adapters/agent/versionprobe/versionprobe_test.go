package versionprobe

import "testing"

func TestFirstOutputLine(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty", []byte(""), ""},
		{"single line", []byte("1.2.3\n"), "1.2.3"},
		{"multi line", []byte("1.2.3\nsome extra info\n"), "1.2.3"},
		{"ansi stripped", []byte("\x1b[32m1.2.3\x1b[0m\n"), "1.2.3"},
		{"leading blank lines", []byte("\n\n1.2.3\n"), "1.2.3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FirstOutputLine(tc.in); got != tc.want {
				t.Errorf("FirstOutputLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCLIVersionMissingBinary(t *testing.T) {
	_, err := CLIVersion(t.Context(), "ao-versionprobe-does-not-exist", "--version")
	if err == nil {
		t.Fatal("CLIVersion: want error for missing binary, got nil")
	}
}
