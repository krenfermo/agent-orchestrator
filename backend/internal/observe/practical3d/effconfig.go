package practical3d

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// EffectiveConfigSchemaV1 is the only effective_config_schema this runner
// accepts. It closes every parameter a provider/SDK/client could otherwise
// take by default (06 §3.3): each key is mandatory, nullable keys must carry
// an explicit null, unknown keys are rejected, and the SDK's transparent retry
// policy is serialized as zero (06 §3.4). Sampling values are decimal strings
// because floats are not canonical manifest numbers.
const EffectiveConfigSchemaV1 = "ao.3d-practical.effective-config.v1"

type effectiveConfigV1 struct {
	SDKMaxRetries         *int64          `json:"sdk_max_retries"`
	Streaming             *bool           `json:"streaming"`
	MaxOutputTokens       json.RawMessage `json:"max_output_tokens"`
	ContextWindowTokens   json.RawMessage `json:"context_window_tokens"`
	Temperature           json.RawMessage `json:"temperature"`
	TopP                  json.RawMessage `json:"top_p"`
	ReasoningEffort       json.RawMessage `json:"reasoning_effort"`
	Tools                 *[]string       `json:"tools"`
	Compaction            *string         `json:"compaction"`
	RequestTimeoutSeconds *int64          `json:"request_timeout_seconds"`
	RequestHeaders        *[]ConfigInput  `json:"request_headers"`
	SystemPromptSHA256    json.RawMessage `json:"system_prompt_sha256"`
}

// EffectiveConfigSchemaClaudeCodeV1 closes the effective configuration of a
// Claude Code agent observed at the AO provider proxy. Every key is checked
// against the bytes of each real request (model, stream, tool policy) or
// against the frozen execution environment (CLI version, permission mode).
const EffectiveConfigSchemaClaudeCodeV1 = "ao.3d-practical.effective-config.claude-code.v1"

// ToolPolicyNoMCPNoWeb forbids MCP and web tools in every request.
const ToolPolicyNoMCPNoWeb = "no_mcp_no_web"

type effectiveConfigClaudeCodeV1 struct {
	ClaudeCodeVersion string `json:"claude_code_version"`
	Model             string `json:"model"`
	Stream            *bool  `json:"stream"`
	ToolPolicy        string `json:"tool_policy"`
	PermissionMode    string `json:"permission_mode"`
}

func decodeClaudeCodeConfig(raw json.RawMessage) (effectiveConfigClaudeCodeV1, error) {
	var c effectiveConfigClaudeCodeV1
	if err := strictUnmarshal(raw, &c); err != nil {
		return c, err
	}
	if c.ClaudeCodeVersion == "" || c.Model == "" || c.Stream == nil || c.ToolPolicy != ToolPolicyNoMCPNoWeb || c.PermissionMode == "" {
		return c, errors.New("claude-code effective config is incomplete")
	}
	return c, nil
}

// EffectiveConfigSchemaCodexV1 closes the effective configuration of a Codex
// CLI agent observed at the AO provider proxy (Responses API).
const EffectiveConfigSchemaCodexV1 = "ao.3d-practical.effective-config.codex.v1"

type effectiveConfigCodexV1 struct {
	CodexVersion string `json:"codex_version"`
	Model        string `json:"model"`
	Stream       *bool  `json:"stream"`
	ToolPolicy   string `json:"tool_policy"`
	SandboxMode  string `json:"sandbox_mode"`
}

func decodeCodexConfig(raw json.RawMessage) (effectiveConfigCodexV1, error) {
	var c effectiveConfigCodexV1
	if err := strictUnmarshal(raw, &c); err != nil {
		return c, err
	}
	if c.CodexVersion == "" || c.Model == "" || c.Stream == nil || c.ToolPolicy != ToolPolicyNoMCPNoWeb || c.SandboxMode == "" {
		return c, errors.New("codex effective config is incomplete")
	}
	return c, nil
}

// checkEffectiveHTTPConfig compares a real request with its frozen cell config.
func checkEffectiveHTTPConfig(cfg InvocationConfig, req HTTPAttemptRequest) error {
	var model string
	var stream bool
	switch cfg.EffectiveConfigSchema {
	case EffectiveConfigSchemaClaudeCodeV1:
		c, err := decodeClaudeCodeConfig(cfg.EffectiveConfig)
		if err != nil {
			return err
		}
		model, stream = c.Model, *c.Stream
	case EffectiveConfigSchemaCodexV1:
		c, err := decodeCodexConfig(cfg.EffectiveConfig)
		if err != nil {
			return err
		}
		model, stream = c.Model, *c.Stream
	default:
		return fmt.Errorf("cell config schema %q cannot describe a real provider request", cfg.EffectiveConfigSchema)
	}
	if model != cfg.ModelID || model != req.Model || stream != req.Stream {
		return fmt.Errorf("request model/stream differ from the frozen effective config")
	}
	return nil
}

func validateEffectiveConfig(schema string, raw json.RawMessage) error {
	if schema == EffectiveConfigSchemaClaudeCodeV1 {
		_, err := decodeClaudeCodeConfig(raw)
		return err
	}
	if schema == EffectiveConfigSchemaCodexV1 {
		_, err := decodeCodexConfig(raw)
		return err
	}
	if schema != EffectiveConfigSchemaV1 {
		return fmt.Errorf("unknown effective_config_schema %q", schema)
	}
	var c effectiveConfigV1
	if err := strictUnmarshal(raw, &c); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return err
	}
	for _, k := range []string{"sdk_max_retries", "streaming", "max_output_tokens", "context_window_tokens", "temperature", "top_p", "reasoning_effort", "tools", "compaction", "request_timeout_seconds", "request_headers", "system_prompt_sha256"} {
		if _, ok := keys[k]; !ok {
			return fmt.Errorf("effective_config missing explicit %q", k)
		}
	}
	if c.SDKMaxRetries == nil || *c.SDKMaxRetries != 0 {
		return errors.New("sdk_max_retries must be explicitly 0")
	}
	if c.Streaming == nil || c.Tools == nil || c.Compaction == nil || *c.Compaction == "" || c.RequestTimeoutSeconds == nil || *c.RequestTimeoutSeconds <= 0 || c.RequestHeaders == nil {
		return errors.New("non-nullable effective_config parameter is null or invalid")
	}
	for _, f := range []struct {
		name string
		raw  json.RawMessage
		kind string
	}{
		{"max_output_tokens", c.MaxOutputTokens, "uint"},
		{"context_window_tokens", c.ContextWindowTokens, "uint"},
		{"temperature", c.Temperature, "decimal"},
		{"top_p", c.TopP, "decimal"},
		{"reasoning_effort", c.ReasoningEffort, "string"},
		{"system_prompt_sha256", c.SystemPromptSHA256, "sha256"},
	} {
		if err := checkNullable(f.raw, f.kind); err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
	}
	if !sort.StringsAreSorted(*c.Tools) || hasDuplicate(*c.Tools) {
		return errors.New("tools must be sorted and unique")
	}
	if err := validateConfigInputs(*c.RequestHeaders); err != nil {
		return fmt.Errorf("request_headers: %w", err)
	}
	return nil
}

func checkNullable(raw json.RawMessage, kind string) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	switch kind {
	case "uint":
		var v int64
		if err := json.Unmarshal(raw, &v); err != nil || v < 0 {
			return errors.New("must be null or a non-negative integer")
		}
	case "decimal":
		var v string
		if err := json.Unmarshal(raw, &v); err != nil || (!canonicalDecimal.MatchString(v) && !canonicalInteger.MatchString(v)) {
			return errors.New("must be null or a canonical decimal string")
		}
	case "string":
		var v string
		if err := json.Unmarshal(raw, &v); err != nil || v == "" {
			return errors.New("must be null or a non-empty string")
		}
	case "sha256":
		var v string
		if err := json.Unmarshal(raw, &v); err != nil || !validSHA256(v) {
			return errors.New("must be null or a sha256")
		}
	}
	return nil
}

func hasDuplicate(xs []string) bool {
	seen := map[string]bool{}
	for _, x := range xs {
		if seen[x] {
			return true
		}
		seen[x] = true
	}
	return false
}
