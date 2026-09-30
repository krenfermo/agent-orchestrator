package practical3d

import (
	"context"
	"debug/buildinfo"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
)

// EnvironmentObserver recomputes the allowlisted environment inputs.
type EnvironmentObserver interface {
	Observe(context.Context) (EnvironmentInputs, error)
}

// StaticEnvironmentObserver returns fixed inputs (tests, technical runs).
type StaticEnvironmentObserver struct {
	Inputs EnvironmentInputs
	Err    error
}

// Observe returns the fixed inputs.
func (s StaticEnvironmentObserver) Observe(context.Context) (EnvironmentInputs, error) {
	return s.Inputs, s.Err
}

// LiveEnvironmentObserver measures the host: binaries, versions, allowlisted env.
type LiveEnvironmentObserver struct {
	Expected     EnvironmentInputs
	AOBinaryPath string
}

// Observe measures every allowlisted input without secrets.
func (o LiveEnvironmentObserver) Observe(ctx context.Context) (EnvironmentInputs, error) {
	out := o.Expected
	// Copies keep empty-but-present lists non-nil (a nil list is "absent").
	out.RuntimeVersions = append(make([]VersionInput, 0, len(out.RuntimeVersions)), out.RuntimeVersions...)
	out.ProviderClientCLIVersions = append(make([]VersionInput, 0, len(out.ProviderClientCLIVersions)), out.ProviderClientCLIVersions...)
	out.TaskToolVersions = append(make([]VersionInput, 0, len(out.TaskToolVersions)), out.TaskToolVersions...)
	out.RunnerInstrumentVersions = append(make([]VersionInput, 0, len(out.RunnerInstrumentVersions)), out.RunnerInstrumentVersions...)
	out.EffectiveEnvironmentConfigAllowlist = append(make([]ConfigInput, 0, len(out.EffectiveEnvironmentConfigAllowlist)), out.EffectiveEnvironmentConfigAllowlist...)
	out.AdditionalLocalConfiguration = append(make([]ConfigInput, 0, len(out.AdditionalLocalConfiguration)), out.AdditionalLocalConfiguration...)
	out.OSPlatformArch = OSPlatformArch{OS: runtime.GOOS, Platform: runtime.GOOS, Arch: runtime.GOARCH}
	self, err := os.Executable()
	if err != nil {
		return EnvironmentInputs{}, err
	}
	aoBinary := o.AOBinaryPath
	if aoBinary == "" {
		aoBinary = self
	}
	bytes, err := os.ReadFile(aoBinary)
	if err != nil {
		return EnvironmentInputs{}, err
	}
	out.AOBinarySHA256 = sha256Hex(bytes)
	out.AOCommit = buildCommitFromFile(aoBinary)
	for _, list := range []*[]VersionInput{&out.RuntimeVersions, &out.ProviderClientCLIVersions, &out.TaskToolVersions, &out.RunnerInstrumentVersions} {
		for i := range *list {
			observed, err := observeVersion(ctx, (*list)[i], self)
			if err != nil {
				return EnvironmentInputs{}, err
			}
			(*list)[i] = observed
		}
	}
	for _, list := range []*[]ConfigInput{&out.EffectiveEnvironmentConfigAllowlist, &out.AdditionalLocalConfiguration} {
		for i := range *list {
			name := (*list)[i].Name
			value, ok := os.LookupEnv(name)
			if !ok {
				return EnvironmentInputs{}, fmt.Errorf("environment allowlist entry %q is unset", name)
			}
			if validSHA256((*list)[i].EffectiveValueOrSHA256) {
				value = sha256Hex([]byte(value))
			}
			(*list)[i].EffectiveValueOrSHA256 = value
		}
	}
	return out, nil
}

func observeVersion(ctx context.Context, expected VersionInput, self string) (VersionInput, error) {
	var path string
	if expected.Component == "ao3dpractical" {
		path = self
	} else {
		var err error
		path, err = exec.LookPath(expected.Component)
		if err != nil {
			return VersionInput{}, fmt.Errorf("locate %s: %w", expected.Component, err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return VersionInput{}, err
	}
	version := expected.Version
	if expected.Component == "ao3dpractical" {
		// The harness's own version is its VCS revision (empty is invalid).
		version = "ao3dpractical@" + buildCommitFromFile(self)
	}
	if expected.Component != "ao3dpractical" {
		flag := "--version"
		if expected.Component == "go" {
			flag = "version"
		}
		cmd := exec.CommandContext(ctx, path, flag)
		raw, err := cmd.CombinedOutput()
		if err != nil {
			return VersionInput{}, fmt.Errorf("%s --version: %w", expected.Component, err)
		}
		version = strings.TrimSpace(string(raw))
	}
	return VersionInput{Component: expected.Component, Version: version, BinarySHA256: sha256Hex(b)}, nil
}

func buildCommit() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && validGitCommit(s.Value) {
				return s.Value
			}
		}
	}
	return ""
}

func buildCommitFromFile(path string) string {
	if info, err := buildinfo.ReadFile(path); err == nil {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && validGitCommit(s.Value) {
				return s.Value
			}
		}
	}
	return buildCommit()
}

// EnvironmentDigest is SHA256(canonical_bytes({digest_schema_version,inputs})).
func EnvironmentDigest(schema string, inputs EnvironmentInputs) (string, error) {
	payload := struct {
		DigestSchemaVersion string            `json:"digest_schema_version"`
		Inputs              EnvironmentInputs `json:"inputs"`
	}{schema, inputs}
	b, err := CanonicalJSON(payload)
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
}
