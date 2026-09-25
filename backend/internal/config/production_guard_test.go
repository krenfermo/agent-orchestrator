package config

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// production_guard_test.go -- the default-data-dir guardrail (Frente 3,
// human decision "option 1"). HOME is always a temporary directory here, so
// the "default" data dir under test is never the operator's real one.

func envOf(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

func TestAuthorizeDefaultDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	def := filepath.Join(home, ".ao", "data")
	for name, tc := range map[string]struct {
		cfg     Config
		env     map[string]string
		allowed bool
	}{
		"1: no owner, no explicit dir => fail closed":        {Config{DataDir: def}, nil, false},
		"2: explicit scratch dir":                            {Config{DataDir: filepath.Join(home, "scratch"), DataDirExplicit: true}, nil, true},
		"2b: default spelled explicitly (AO_DATA_DIR)":       {Config{DataDir: def, DataDirExplicit: true}, nil, true},
		"3: desktop contract app":                            {Config{DataDir: def}, map[string]string{"AO_OWNER": "app", "AO_APP_RUN_ID": "apprun-1"}, true},
		"3b: desktop contract persistent":                    {Config{DataDir: def}, map[string]string{"AO_OWNER": "persistent", "AO_APP_RUN_ID": "apprun-1"}, true},
		"owner without run id":                               {Config{DataDir: def}, map[string]string{"AO_OWNER": "app"}, false},
		"owner with blank run id":                            {Config{DataDir: def}, map[string]string{"AO_OWNER": "app", "AO_APP_RUN_ID": "  "}, false},
		"arbitrary non-empty owner is not the contract":      {Config{DataDir: def}, map[string]string{"AO_OWNER": "headless", "AO_APP_RUN_ID": "x"}, false},
		"owner spelled differently":                          {Config{DataDir: def}, map[string]string{"AO_OWNER": "APP", "AO_APP_RUN_ID": "x"}, false},
		"owner with whitespace":                              {Config{DataDir: def}, map[string]string{"AO_OWNER": " app", "AO_APP_RUN_ID": "x"}, false},
		"run id alone":                                       {Config{DataDir: def}, map[string]string{"AO_APP_RUN_ID": "apprun-1"}, false},
		"non-default dir not marked explicit (defensive ok)": {Config{DataDir: filepath.Join(home, "other")}, nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			err := AuthorizeDefaultDataDir(tc.cfg, envOf(tc.env))
			if tc.allowed && err != nil {
				t.Fatalf("want allowed, got %v", err)
			}
			if !tc.allowed && !errors.Is(err, ErrDefaultDataDirUnauthorized) {
				t.Fatalf("want ErrDefaultDataDirUnauthorized, got %v", err)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(home, ".ao")); !os.IsNotExist(err) {
		t.Fatalf("the check created %s/.ao", home)
	}
}

func TestLoadMarksOnlyAnExplicitDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AO_DATA_DIR", "")
	os.Unsetenv("AO_DATA_DIR")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDirExplicit || cfg.DataDir != filepath.Join(home, ".ao", "data") {
		t.Fatalf("defaulted data dir: explicit=%v dir=%s", cfg.DataDirExplicit, cfg.DataDir)
	}
	t.Setenv("AO_DATA_DIR", filepath.Join(home, "scratch"))
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DataDirExplicit {
		t.Fatal("AO_DATA_DIR must mark the data dir explicit")
	}
}

// Requirement 7: the guard depends on a signal the desktop app really sends.
// This reads the Electron sources the packaged app is built from and proves
// that (a) every AO_OWNER value daemonEnv can produce is one the guard
// accepts, (b) AO_APP_RUN_ID is always set beside it and never empty, and
// (c) the daemon is spawned with that environment -- as `ao daemon` in the
// packaged build.
func TestElectronLaunchContractSatisfiesTheGuard(t *testing.T) {
	root := filepath.Join("..", "..", "..", "frontend", "src")
	mainTS := readSource(t, filepath.Join(root, "main.ts"))
	launchTS := readSource(t, filepath.Join(root, "shared", "daemon-launch.ts"))

	m := regexp.MustCompile(`const AO_OWNER = forceKeep \? "([a-z]+)" : "([a-z]+)";`).FindStringSubmatch(mainTS)
	if m == nil {
		t.Fatal("frontend/src/main.ts no longer derives AO_OWNER the way the guard expects; re-check the contract")
	}
	for _, owner := range m[1:] {
		if !desktopOwners[owner] {
			t.Fatalf("Electron can send AO_OWNER=%q, which the guard would refuse", owner)
		}
	}
	for owner := range desktopOwners {
		if owner != m[1] && owner != m[2] {
			t.Fatalf("the guard accepts AO_OWNER=%q, which Electron never sends", owner)
		}
	}
	if !regexp.MustCompile(`const appRunId = process\.env\.AO_APP_RUN_ID \?\? ` + "`apprun-\\$\\{randomUUID\\(\\)\\}`").MatchString(mainTS) {
		t.Fatal("AO_APP_RUN_ID is no longer minted as apprun-<uuid>; re-check the contract")
	}
	ownerTag := mainTS[strings.Index(mainTS, "const ownerTag = {"):]
	ownerTag = ownerTag[:strings.Index(ownerTag, "};")]
	if !strings.Contains(ownerTag, "AO_OWNER,") || !strings.Contains(ownerTag, "AO_APP_RUN_ID: appRunId,") {
		t.Fatalf("ownerTag no longer carries AO_OWNER and AO_APP_RUN_ID together:\n%s", ownerTag)
	}
	if !strings.Contains(mainTS, "env: daemonEnv(keep)") {
		t.Fatal("the daemon spawn no longer passes daemonEnv(); the guard's signal would not reach it")
	}
	if !strings.Contains(launchTS, `args: ["daemon"],`) {
		t.Fatal("the packaged daemon is no longer launched as `ao daemon`")
	}
	// Dev mode names the data dir explicitly anyway (AO_DATA_DIR under ~/.ao/dev).
	if !strings.Contains(mainTS, `if (!process.env.AO_DATA_DIR) devExtras.AO_DATA_DIR =`) {
		t.Fatal("Electron dev mode no longer sets AO_DATA_DIR explicitly")
	}
}

func readSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
