package main

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// The command's contract in its healthy state: the shipped router changes no
// outcome, so the harness exits zero and still reports what it measured.
func TestHarnessExitsZeroAndReportsTheMeasuredReduction(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	// The fixture's code graph and memory stores are written under AO's data
	// dir; pin it into the test's temp tree so a test run never touches a real
	// ~/.ao.
	t.Setenv("AO_DATA_DIR", t.TempDir())

	var out bytes.Buffer
	code, err := run(nil, &out)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if code != 0 {
		t.Fatalf("exit code %d, want 0:\n%s", code, out.String())
	}
	for _, want := range []string{
		"router disabled",
		"router enabled",
		"measured context reduction:",
		"no quality regression",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("report does not contain %q:\n%s", want, out.String())
		}
	}
}

// A bad flag is a usage error, not a regression verdict.
func TestUnknownFlagFails(t *testing.T) {
	var out bytes.Buffer
	if _, err := run([]string{"-nope"}, &out); err == nil {
		t.Fatal("an unknown flag was accepted")
	}
}
