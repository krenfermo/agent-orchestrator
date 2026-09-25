package usage

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/repoaccess"
)

// exploration.go -- Frente 3 / 3C: tool observations from the transcript the
// usage parser already tails.
//
// turn_class.go reads a tool call's NAME and nothing else. This file reads one
// more thing, and only for the tools whose purpose is to name a file: the
// target path. That path is normalised against the project root AO itself
// recorded for the subject, run through the 3B repository boundary, and kept
// only when it lies inside the project and is neither a secret nor under an
// excluded directory. Every other path is reduced to a scope word. Search
// patterns, commands, arguments and results never leave this file: a command
// is read only to name its leading PROGRAM, and a result only to measure its
// length.

// explorationScope is what the parser needs to place a path. It is built once
// per ingestion from facts AO owns, never from the transcript alone: a
// transcript's own `cwd` is trusted as a base for relative paths only when it
// lies inside a root AO recorded.
type explorationScope struct {
	roots       []string
	harnessHome string
}

// newExplorationScope builds the scope for one source. root is the workspace
// AO recorded for the binding's subject (empty when AO has none, in which case
// every path is unresolved rather than guessed). artifactPath is the
// transcript's own location, whose ancestors name the harness home.
func newExplorationScope(root, artifactPath string, kind domain.UsageSourceKind) explorationScope {
	scope := explorationScope{}
	if root = strings.TrimSpace(root); root != "" && filepath.IsAbs(root) {
		clean := filepath.Clean(root)
		scope.roots = append(scope.roots, clean)
		// macOS reaches /var through /private/var and /tmp through
		// /private/tmp; a harness reports whichever spelling its cwd had. The
		// resolved form is added so the same file is not counted as outside
		// the project under its other name.
		if resolved, err := filepath.EvalSymlinks(clean); err == nil && resolved != clean {
			scope.roots = append(scope.roots, resolved)
		}
	}
	scope.harnessHome = harnessHomeOf(artifactPath, kind)
	return scope
}

// harnessHomeOf derives the harness configuration home from where its
// transcript lives: `<home>/projects/<slug>/...` for Claude and
// `<home>/sessions/YYYY/MM/DD/...` for Codex. Empty when the layout does not
// match, which only costs the outside_harness/outside distinction.
func harnessHomeOf(artifactPath string, kind domain.UsageSourceKind) string {
	marker := "projects"
	if kind == domain.UsageSourceCodexRollout {
		marker = "sessions"
	}
	dir := filepath.Dir(filepath.Clean(artifactPath))
	for dir != "" && dir != filepath.Dir(dir) {
		if filepath.Base(dir) == marker {
			return filepath.Dir(dir)
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

// within reports p's path relative to root when p is root or lies beneath it.
func within(root, p string) (string, bool) {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

// baseFor returns a transcript-reported cwd when it lies inside a known root,
// so a relative tool path can be placed. Anything else is not a trustworthy
// base.
func (s explorationScope) baseFor(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	cwd = filepath.Clean(cwd)
	for _, root := range s.roots {
		if _, ok := within(root, cwd); ok {
			return cwd
		}
	}
	return ""
}

// maxRawToolPathBytes refuses to even normalise an implausible path value.
const maxRawToolPathBytes = 4096

// classify places one raw path. The returned path is non-empty only for
// ToolPathProject.
func (s explorationScope) classify(raw, base string) (domain.ToolPathScope, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxRawToolPathBytes || strings.ContainsRune(raw, 0) {
		return domain.ToolPathUnresolved, ""
	}
	if strings.HasPrefix(raw, "~") {
		// Home-relative. AO does not know the agent's HOME for certain, and
		// nothing in a project is spelled this way.
		return domain.ToolPathOutside, ""
	}
	p := raw
	if !filepath.IsAbs(p) {
		if base == "" {
			return domain.ToolPathUnresolved, ""
		}
		p = filepath.Join(base, p)
	}
	p = filepath.Clean(p)
	for _, root := range s.roots {
		if rel, ok := within(root, p); ok {
			return classifyProjectRel(filepath.ToSlash(rel))
		}
	}
	if s.harnessHome != "" {
		if _, ok := within(s.harnessHome, p); ok {
			return domain.ToolPathOutsideHarness, ""
		}
	}
	if len(s.roots) == 0 {
		// Without a root AO cannot tell inside from outside, and saying
		// "outside" would be a claim it has no basis for.
		return domain.ToolPathUnresolved, ""
	}
	return domain.ToolPathOutside, ""
}

// classifyProjectRel applies the path-only half of the 3B boundary to a path
// already known to lie inside the project. The file is never opened: the
// worktree may be gone by the time a transcript is ingested, and the question
// is where the agent looked, not what is there now.
func classifyProjectRel(rel string) (domain.ToolPathScope, string) {
	if rel == "." {
		return domain.ToolPathProject, "."
	}
	if err := repoaccess.CheckPath(rel); err != nil {
		switch {
		case errors.Is(err, repoaccess.ErrSecretPath):
			return domain.ToolPathSecret, ""
		case errors.Is(err, repoaccess.ErrExcluded):
			return domain.ToolPathExcluded, ""
		default:
			return domain.ToolPathUnresolved, ""
		}
	}
	if len(rel) > domain.MaxToolObservationPathBytes {
		return domain.ToolPathUnresolved, ""
	}
	// A path whose own spelling looks like a credential (a token used as a
	// directory name) is treated as the secret it resembles.
	if redacted, n := repoaccess.Redact(rel); n > 0 || redacted != rel {
		return domain.ToolPathSecret, ""
	}
	return domain.ToolPathProject, rel
}

// toolOpOf maps a tool name to its operation. Exact and case-sensitive, for
// the reason turn_class.go gives: "BashOutput" contains "Bash" and means the
// opposite thing.
var toolOpOf = map[string]domain.ToolOp{
	// Claude Code.
	"Read":                        domain.ToolOpRead,
	"NotebookRead":                domain.ToolOpRead,
	"Grep":                        domain.ToolOpSearch,
	"Glob":                        domain.ToolOpList,
	"LS":                          domain.ToolOpList,
	"Edit":                        domain.ToolOpEdit,
	"MultiEdit":                   domain.ToolOpEdit,
	"Write":                       domain.ToolOpEdit,
	"NotebookEdit":                domain.ToolOpEdit,
	"str_replace_editor":          domain.ToolOpEdit,
	"str_replace_based_edit_tool": domain.ToolOpEdit,
	"Bash":                        domain.ToolOpCommand,
	"BashOutput":                  domain.ToolOpWait,
	"KillBash":                    domain.ToolOpWait,
	"KillShell":                   domain.ToolOpWait,
	"Task":                        domain.ToolOpDelegate,
	"Agent":                       domain.ToolOpDelegate,
	"TodoWrite":                   domain.ToolOpPlan,
	"ExitPlanMode":                domain.ToolOpPlan,
	"EnterPlanMode":               domain.ToolOpPlan,
	"WebFetch":                    domain.ToolOpWeb,
	"WebSearch":                   domain.ToolOpWeb,
	// Codex.
	"shell":             domain.ToolOpCommand,
	"exec_command":      domain.ToolOpCommand,
	"local_shell":       domain.ToolOpCommand,
	"container.exec":    domain.ToolOpCommand,
	"exec":              domain.ToolOpCommand,
	"js":                domain.ToolOpCommand,
	"apply_patch":       domain.ToolOpEdit,
	"write_stdin":       domain.ToolOpWait,
	"wait":              domain.ToolOpWait,
	"spawn_agent":       domain.ToolOpDelegate,
	"update_plan":       domain.ToolOpPlan,
	"web_search":        domain.ToolOpWeb,
	"view_image":        domain.ToolOpRead,
	"web_search_call":   domain.ToolOpWeb,
	"local_shell_call":  domain.ToolOpCommand,
	"image_generation":  domain.ToolOpOther,
	"mcp_tool_call_end": domain.ToolOpOther,
}

func opOfTool(name string) domain.ToolOp {
	if op, ok := toolOpOf[name]; ok {
		return op
	}
	return domain.ToolOpOther
}

// exploreProgram is the set of leading programs whose purpose is to inspect.
// A command is classified from its first program only; `rg x | xargs rm`
// would be misread, which is why the class it yields is DERIVED and why the
// read model never presents it as a measured file read.
var exploreProgram = map[string]bool{
	"cat": true, "head": true, "tail": true, "less": true, "more": true, "bat": true, "nl": true,
	"rg": true, "grep": true, "egrep": true, "fgrep": true, "ag": true, "ack": true,
	"find": true, "fd": true, "ls": true, "tree": true, "wc": true, "file": true, "stat": true,
	"du": true, "jq": true, "sed": true,
}

// editProgram is the set of programs whose purpose is to write files.
var editProgram = map[string]bool{"tee": true, "patch": true}

// exploreGitSubcommand is the set of git subcommands that only inspect.
var exploreGitSubcommand = map[string]bool{
	"grep": true, "log": true, "show": true, "diff": true, "ls-files": true, "blame": true,
	"status": true, "ls-tree": true, "cat-file": true, "rev-parse": true, "branch": true,
}

// commandOp classifies a shell command by its leading program. The command
// text is inspected here and dropped here.
func commandOp(command string) domain.ToolOp {
	fields := leadingCommandFields(command)
	if len(fields) == 0 {
		return domain.ToolOpCommand
	}
	if writesThroughRedirect(command) {
		return domain.ToolOpCommandEdit
	}
	program := filepath.Base(fields[0])
	for _, segment := range strings.FieldsFunc(command, func(r rune) bool { return r == ';' || r == '&' || r == '|' || r == '\n' }) {
		if f := strings.Fields(segment); len(f) > 0 && editProgram[filepath.Base(f[0])] {
			return domain.ToolOpCommandEdit
		}
	}
	switch {
	case program == "git":
		for i := 1; i < len(fields); i++ {
			f := fields[i]
			if f == "-C" || f == "-c" || f == "--git-dir" || f == "--work-tree" {
				i++ // the option's value is not the subcommand
				continue
			}
			if strings.HasPrefix(f, "-") {
				continue
			}
			if exploreGitSubcommand[f] {
				return domain.ToolOpCommandExplore
			}
			if f == "apply" {
				return domain.ToolOpCommandEdit
			}
			return domain.ToolOpCommand
		}
		return domain.ToolOpCommand
	case program == "sed":
		// `sed -n 'a,bp' f` reads; `sed -i` writes.
		readOnly := false
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "-i") || f == "--in-place" {
				return domain.ToolOpCommandEdit
			}
			if f == "-n" {
				readOnly = true
			}
		}
		if readOnly {
			return domain.ToolOpCommandExplore
		}
		return domain.ToolOpCommand
	case exploreProgram[program]:
		for _, f := range fields[1:] {
			// find -delete / -exec can mutate.
			if f == "-delete" || f == "-exec" || f == "-execdir" {
				return domain.ToolOpCommand
			}
		}
		return domain.ToolOpCommandExplore
	}
	return domain.ToolOpCommand
}

// leadingCommandFields returns the whitespace fields of the first segment that
// is not a bare `cd`, with leading VAR=value assignments removed.
func leadingCommandFields(command string) []string {
	segments := strings.FieldsFunc(command, func(r rune) bool { return r == ';' || r == '&' || r == '|' || r == '\n' })
	for _, segment := range segments {
		fields := strings.Fields(segment)
		for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "=") {
			fields = fields[1:]
		}
		if len(fields) == 0 || fields[0] == "cd" {
			continue
		}
		return fields
	}
	return nil
}

// writesThroughRedirect reports an output redirection to anything other than
// /dev/null or another descriptor -- `cat > f <<EOF` is a write.
func writesThroughRedirect(command string) bool {
	stripped := command
	for _, benign := range []string{"2>&1", "1>&2", ">&2", "2>/dev/null", ">/dev/null", "> /dev/null", "2> /dev/null", "&>/dev/null"} {
		stripped = strings.ReplaceAll(stripped, benign, " ")
	}
	return strings.Contains(stripped, ">")
}

// claudeToolRecord is the second, narrow decode of a transcript record that
// exploration needs. It is kept apart from claudeTranscriptRecord on purpose:
// that type's content blocks are deliberately unable to carry `input`, and
// widening it would weaken a guarantee turn classification relies on.
type claudeToolRecord struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	Timestamp   string `json:"timestamp"`
	Cwd         string `json:"cwd"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	IsCompact   bool   `json:"isCompactSummary"`
	Message     struct {
		ID string `json:"id"`
		// Content is either a string (a typed prompt) or a list of blocks.
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	Attachment    json.RawMessage `json:"attachment"`
}

// claudeToolBlock is one content block, decoded down to what exploration
// reads. Input fields are RawMessage so a tool whose `path` is not a string
// costs that one field, not the whole record.
type claudeToolBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Text      json.RawMessage `json:"text"`
	Content   json.RawMessage `json:"content"`
	Input     struct {
		FilePath     json.RawMessage `json:"file_path"`
		NotebookPath json.RawMessage `json:"notebook_path"`
		Path         json.RawMessage `json:"path"`
		Command      json.RawMessage `json:"command"`
	} `json:"input"`
}

// rawString decodes a JSON string value, or returns "" for anything else.
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// textBytes measures the text a content value carries: a string, or a list of
// blocks with `text` (Claude) fields. Images and other non-text blocks
// contribute nothing; their size is not text the model reads as such.
func textBytes(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	switch raw[0] {
	case '"':
		s := rawString(raw)
		return int64(len(s)), true
	case '[':
		var blocks []struct {
			Type string          `json:"type"`
			Text json.RawMessage `json:"text"`
		}
		if json.Unmarshal(raw, &blocks) != nil {
			return 0, false
		}
		var total int64
		for _, b := range blocks {
			total += int64(len(rawString(b.Text)))
		}
		return total, true
	}
	return 0, false
}

// claudeResultItems reads the harness's own count of what a read or search
// returned. Nothing else of toolUseResult is decoded.
func claudeResultItems(raw json.RawMessage) *int64 {
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var shape struct {
		File *struct {
			NumLines *int64 `json:"numLines"`
		} `json:"file"`
		NumFiles *int64 `json:"numFiles"`
	}
	if json.Unmarshal(raw, &shape) != nil {
		return nil
	}
	if shape.File != nil && shape.File.NumLines != nil && *shape.File.NumLines >= 0 {
		return shape.File.NumLines
	}
	if shape.NumFiles != nil && *shape.NumFiles >= 0 {
		return shape.NumFiles
	}
	return nil
}

// toolTargetOf picks the one input field that names a file for a tool, and
// whether the tool's target defaults to the project root when absent. A field
// that is present but not a string is not "absent": it yields an empty raw
// value with defaultsToRoot false, so it is recorded as unresolved.
func toolTargetOf(name string, b claudeToolBlock) (raw string, defaultsToRoot, names bool) {
	switch name {
	case "Read", "Edit", "MultiEdit", "Write":
		return rawString(b.Input.FilePath), false, true
	case "NotebookRead", "NotebookEdit":
		return rawString(b.Input.NotebookPath), false, true
	case "Grep", "Glob", "LS":
		absent := len(b.Input.Path) == 0 || string(b.Input.Path) == "null"
		return rawString(b.Input.Path), absent, true
	}
	return "", false, false
}

// claudeObservationKey is the exactly-once identity of a Claude observation.
func claudeObservationKey(source domain.UsageSourceContext, kind, id string) string {
	return stableSourceEventKey(
		"claude-obs",
		source.NativeRootID,
		string(source.Source.Kind),
		source.Source.SubagentID,
		source.Source.NativeSessionID,
		kind,
		id,
	)
}

// observeClaudeRecord extracts every tool observation and result one Claude
// transcript record carries. A tool call carries the SourceEventKey of the
// billed message it belongs to, derived exactly as parseClaude derives it.
func observeClaudeRecord(
	source domain.UsageSourceContext,
	scope explorationScope,
	record jsonlRecord,
	facts *domain.AgentToolFacts,
) {
	var native claudeToolRecord
	if json.Unmarshal(record.Data, &native) != nil {
		return
	}
	if source.Source.Kind == domain.UsageSourceClaudeMain && native.IsSidechain {
		return
	}
	observedAt := parseObservedAt(native.Timestamp)
	recordID := firstNonEmpty(native.UUID, "offset:"+itoa(record.Offset))
	switch native.Type {
	case "assistant":
		eventKey := claudeSourceEventKey(source, firstNonEmpty(native.Message.ID, native.UUID, itoa(record.Offset)))
		var blocks []claudeToolBlock
		if len(native.Message.Content) == 0 || native.Message.Content[0] != '[' ||
			json.Unmarshal(native.Message.Content, &blocks) != nil {
			return
		}
		base := scope.baseFor(native.Cwd)
		for _, b := range blocks {
			if b.Type != "tool_use" || strings.TrimSpace(b.ID) == "" {
				continue
			}
			obs := domain.AgentToolObservation{
				Key:        claudeObservationKey(source, "tool", b.ID),
				EventKey:   eventKey,
				Ordinal:    record.Offset,
				ObservedAt: observedAt,
				Origin:     domain.OriginAgentExploration,
				Op:         opOfTool(b.Name),
				PathScope:  domain.ToolPathNone,
			}
			if domain.ValidToolName(b.Name) {
				obs.ToolName = b.Name
			}
			if obs.Op == domain.ToolOpCommand && b.Name == "Bash" {
				obs.Op = commandOp(rawString(b.Input.Command))
			}
			if raw, defaultsToRoot, names := toolTargetOf(b.Name, b); names {
				switch {
				case strings.TrimSpace(raw) != "":
					obs.PathScope, obs.Path = scope.classify(raw, base)
				case defaultsToRoot && base != "":
					obs.PathScope, obs.Path = scope.classify(base, "")
				default:
					obs.PathScope = domain.ToolPathUnresolved
				}
			}
			facts.Observations = append(facts.Observations, obs)
		}
	case "user":
		content := native.Message.Content
		if len(content) > 0 && content[0] == '[' {
			var blocks []claudeToolBlock
			if json.Unmarshal(content, &blocks) == nil {
				handled := false
				for _, b := range blocks {
					if b.Type != "tool_result" || strings.TrimSpace(b.ToolUseID) == "" {
						continue
					}
					handled = true
					size, _ := textBytes(b.Content)
					facts.Results = append(facts.Results, domain.AgentToolResult{
						Key:         claudeObservationKey(source, "tool", b.ToolUseID),
						ResultBytes: size,
						ResultItems: claudeResultItems(native.ToolUseResult),
						IsError:     b.IsError,
					})
				}
				if handled {
					return
				}
			}
		}
		size, ok := textBytes(content)
		if !ok {
			return
		}
		obs := domain.AgentToolObservation{
			Key:         claudeObservationKey(source, "user", recordID),
			Ordinal:     record.Offset,
			ObservedAt:  observedAt,
			Origin:      domain.OriginAOContext,
			Op:          domain.ToolOpPrompt,
			ToolName:    "user_message",
			PathScope:   domain.ToolPathNone,
			ResultBytes: &size,
		}
		switch {
		case native.IsCompact:
			obs.Origin, obs.Op, obs.ToolName = domain.OriginHarnessContext, domain.ToolOpInjected, "compact_summary"
		case native.IsMeta:
			obs.Origin, obs.Op, obs.ToolName = domain.OriginHarnessContext, domain.ToolOpInjected, "meta_message"
		}
		facts.Observations = append(facts.Observations, obs)
	case "attachment":
		if len(native.Attachment) == 0 || native.Attachment[0] != '{' {
			return
		}
		var shape struct {
			Type     string `json:"type"`
			Path     string `json:"path"`
			Filename string `json:"filename"`
		}
		if json.Unmarshal(native.Attachment, &shape) != nil {
			return
		}
		size := int64(len(native.Attachment))
		obs := domain.AgentToolObservation{
			Key:         claudeObservationKey(source, "attachment", recordID),
			Ordinal:     record.Offset,
			ObservedAt:  observedAt,
			Origin:      domain.OriginHarnessContext,
			Op:          domain.ToolOpInjected,
			ToolName:    "attachment",
			PathScope:   domain.ToolPathNone,
			ResultBytes: &size,
		}
		if domain.ValidToolName(shape.Type) {
			obs.ToolName = shape.Type
		}
		if target := firstNonEmpty(shape.Path, shape.Filename); target != "" {
			obs.PathScope, obs.Path = scope.classify(target, scope.baseFor(native.Cwd))
		}
		facts.Observations = append(facts.Observations, obs)
	}
}

// codexObservationKey is the exactly-once identity of a Codex observation.
func codexObservationKey(source domain.UsageSourceContext, kind string, parts ...string) string {
	all := append([]string{source.NativeRootID, source.Source.NativeSessionID, kind}, parts...)
	return stableSourceEventKey("codex-obs", all...)
}

// codexCallPayload is a Codex tool call, narrowed. `arguments` is decoded only
// for the structured shell tools, and only to name the leading program.
type codexCallPayload struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	CallID    string          `json:"call_id"`
	Role      string          `json:"role"`
	Arguments json.RawMessage `json:"arguments"`
	Output    json.RawMessage `json:"output"`
	Content   json.RawMessage `json:"content"`
	Action    *struct {
		Command []string `json:"command"`
	} `json:"action"`
}

// codexCommandOf reads the command of a structured shell call: `{"cmd": "..."}`
// or `{"command": ["bash","-lc","..."]}`. The code-mode `exec` tool carries
// JavaScript instead, which AO does not parse.
func codexCommandOf(p codexCallPayload) (string, bool) {
	if p.Action != nil && len(p.Action.Command) > 0 {
		return strings.Join(p.Action.Command, " "), true
	}
	argsText := rawString(p.Arguments)
	if argsText == "" {
		return "", false
	}
	var args struct {
		Cmd     json.RawMessage `json:"cmd"`
		Command json.RawMessage `json:"command"`
	}
	if json.Unmarshal([]byte(argsText), &args) != nil {
		return "", false
	}
	if s := rawString(args.Cmd); s != "" {
		return s, true
	}
	if len(args.Command) > 0 && args.Command[0] == '[' {
		var parts []string
		if json.Unmarshal(args.Command, &parts) == nil && len(parts) > 0 {
			// ["bash","-lc","rg foo"] -- the script is the last element.
			if len(parts) >= 3 && (parts[1] == "-lc" || parts[1] == "-c") {
				return parts[len(parts)-1], true
			}
			return strings.Join(parts, " "), true
		}
	}
	return rawString(args.Command), rawString(args.Command) != ""
}

// codexOutputBytes measures a Codex tool output: a string, or a list of
// `input_text` / `output_text` blocks.
func codexOutputBytes(raw json.RawMessage) (int64, bool) {
	if len(raw) > 0 && raw[0] == '{' {
		var wrapped struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &wrapped) == nil && len(wrapped.Content) > 0 {
			return textBytes(wrapped.Content)
		}
		return 0, false
	}
	return textBytes(raw)
}

// observeCodexResponseItem extracts the observation or result one Codex
// `response_item` carries, and returns the call's operation so the caller can
// fold it into the pending turn class.
func observeCodexResponseItem(
	source domain.UsageSourceContext,
	envelope codexEnvelope,
	offset int64,
	facts *domain.AgentToolFacts,
) (domain.ToolOp, bool) {
	var p codexCallPayload
	if json.Unmarshal(envelope.Payload, &p) != nil {
		return "", false
	}
	observedAt := parseObservedAt(envelope.Timestamp)
	switch p.Type {
	case "function_call", "custom_tool_call", "local_shell_call", "web_search_call":
		name := p.Name
		if name == "" {
			name = p.Type
		}
		callID := strings.TrimSpace(p.CallID)
		if callID == "" {
			callID = "offset:" + itoa(offset)
		}
		obs := domain.AgentToolObservation{
			Key:        codexObservationKey(source, "tool", callID),
			Ordinal:    offset,
			ObservedAt: observedAt,
			Origin:     domain.OriginAgentExploration,
			Op:         opOfTool(name),
			PathScope:  domain.ToolPathNone,
		}
		if domain.ValidToolName(name) {
			obs.ToolName = name
		}
		if obs.Op == domain.ToolOpCommand {
			if command, ok := codexCommandOf(p); ok {
				obs.Op = commandOp(command)
			}
		}
		facts.Observations = append(facts.Observations, obs)
		return obs.Op, true
	case "function_call_output", "custom_tool_call_output":
		if strings.TrimSpace(p.CallID) == "" {
			return "", false
		}
		size, ok := codexOutputBytes(p.Output)
		if !ok {
			return "", false
		}
		facts.Results = append(facts.Results, domain.AgentToolResult{
			Key:         codexObservationKey(source, "tool", p.CallID),
			ResultBytes: size,
		})
	case "message":
		// Developer-role messages are the harness's own instructions. User-
		// role items are NOT recorded: Codex writes AO's prompt there AND
		// injects AGENTS.md / environment context there, and AO cannot tell
		// them apart without reading them. The prompt is taken from the
		// user_message event instead; the injected share stays unmeasured.
		if p.Role != "developer" {
			return "", false
		}
		size, ok := textBytes(p.Content)
		if !ok {
			return "", false
		}
		facts.Observations = append(facts.Observations, domain.AgentToolObservation{
			Key:         codexObservationKey(source, "developer", envelope.Timestamp, itoa(offset)),
			Ordinal:     offset,
			ObservedAt:  observedAt,
			Origin:      domain.OriginHarnessContext,
			Op:          domain.ToolOpInjected,
			ToolName:    "developer_message",
			PathScope:   domain.ToolPathNone,
			ResultBytes: &size,
		})
	}
	return "", false
}

// observeCodexUserMessage records the size of a prompt delivered to Codex.
func observeCodexUserMessage(source domain.UsageSourceContext, envelope codexEnvelope, offset int64, facts *domain.AgentToolFacts) {
	var p struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if json.Unmarshal(envelope.Payload, &p) != nil || p.Type != "user_message" {
		return
	}
	size := int64(len(p.Message))
	facts.Observations = append(facts.Observations, domain.AgentToolObservation{
		Key:         codexObservationKey(source, "user", envelope.Timestamp, itoa(offset)),
		Ordinal:     offset,
		ObservedAt:  parseObservedAt(envelope.Timestamp),
		Origin:      domain.OriginAOContext,
		Op:          domain.ToolOpPrompt,
		ToolName:    "user_message",
		PathScope:   domain.ToolPathNone,
		ResultBytes: &size,
	})
}

// observeCodexSessionMeta records the size of the base instructions Codex
// carries as its system prompt.
func observeCodexSessionMeta(source domain.UsageSourceContext, envelope codexEnvelope, offset int64, facts *domain.AgentToolFacts) {
	var p struct {
		ID               string `json:"id"`
		BaseInstructions *struct {
			Text string `json:"text"`
		} `json:"base_instructions"`
	}
	if json.Unmarshal(envelope.Payload, &p) != nil || p.BaseInstructions == nil {
		return
	}
	size := int64(len(p.BaseInstructions.Text))
	facts.Observations = append(facts.Observations, domain.AgentToolObservation{
		Key:         codexObservationKey(source, "base_instructions", p.ID, itoa(offset)),
		Ordinal:     offset,
		ObservedAt:  parseObservedAt(envelope.Timestamp),
		Origin:      domain.OriginHarnessContext,
		Op:          domain.ToolOpInjected,
		ToolName:    "base_instructions",
		PathScope:   domain.ToolPathNone,
		ResultBytes: &size,
	})
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

// maxPatchPathsPerEvent bounds how many edited paths one patch event may
// contribute, so a malformed record cannot flood the table.
const maxPatchPathsPerEvent = 256

// observeCodexPatchApply records the files a Codex patch changed. It is the
// one structured file signal a Codex rollout carries: `patch_apply_end` lists
// the changed paths as the KEYS of `changes`; the values (the diff) are never
// decoded.
func observeCodexPatchApply(
	source domain.UsageSourceContext,
	scope explorationScope,
	envelope codexEnvelope,
	offset int64,
	facts *domain.AgentToolFacts,
) {
	var p struct {
		Type    string                     `json:"type"`
		CallID  string                     `json:"call_id"`
		Changes map[string]json.RawMessage `json:"changes"`
	}
	if json.Unmarshal(envelope.Payload, &p) != nil || p.Type != "patch_apply_end" {
		return
	}
	callID := firstNonEmpty(p.CallID, "offset:"+itoa(offset))
	paths := make([]string, 0, len(p.Changes))
	for path := range p.Changes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) > maxPatchPathsPerEvent {
		paths = paths[:maxPatchPathsPerEvent]
	}
	for _, path := range paths {
		obs := domain.AgentToolObservation{
			Key:        codexObservationKey(source, "patch", callID, path),
			Ordinal:    offset,
			ObservedAt: parseObservedAt(envelope.Timestamp),
			Origin:     domain.OriginAgentExploration,
			Op:         domain.ToolOpEdit,
			ToolName:   CodexPatchApplyTool,
		}
		obs.PathScope, obs.Path = scope.classify(path, "")
		facts.Observations = append(facts.Observations, obs)
	}
}

// maxParsedCommandsPerItem bounds how many sub-operations one command may
// contribute.
const maxParsedCommandsPerItem = 64

// observeCodexItemCompleted reads the structured items current Codex
// versions record as `event_msg` / `item_completed`:
//
//   - CommandExecution.parsed_cmd -- Codex's OWN parse of the command it ran:
//     `read` (with the file), `search` (with the scope; the query is never
//     decoded), `list_files` (with the directory), `unknown`. It is the
//     provider's structured statement of what the command inspected, so it is
//     recorded as observed exploration without AO parsing any shell text.
//   - FileChange -- the changed paths, as the keys of `changes`; the diffs
//     are never decoded.
//   - UserMessage -- the prompt delivered to the agent, measured, never kept.
//
// Every sub-observation carries a tool name marking it as part of a call
// already counted (the `exec` custom tool call), so a read model never counts
// it as an extra tool call.
func observeCodexItemCompleted(
	source domain.UsageSourceContext,
	scope explorationScope,
	envelope codexEnvelope,
	offset int64,
	facts *domain.AgentToolFacts,
) {
	var p struct {
		Type string `json:"type"`
		Item struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Cwd       string `json:"cwd"`
			ParsedCmd []struct {
				Type string `json:"type"`
				Path string `json:"path"`
				Name string `json:"name"`
			} `json:"parsed_cmd"`
			Changes map[string]json.RawMessage `json:"changes"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"item"`
	}
	if json.Unmarshal(envelope.Payload, &p) != nil || p.Type != "item_completed" {
		return
	}
	itemID := firstNonEmpty(p.Item.ID, "offset:"+itoa(offset))
	observedAt := parseObservedAt(envelope.Timestamp)
	switch p.Item.Type {
	case "CommandExecution":
		base := scope.baseFor(p.Item.Cwd)
		for i, pc := range p.Item.ParsedCmd {
			if i >= maxParsedCommandsPerItem {
				break
			}
			var op domain.ToolOp
			switch pc.Type {
			case "read":
				op = domain.ToolOpRead
			case "search":
				op = domain.ToolOpSearch
			case "list_files":
				op = domain.ToolOpList
			default:
				// Codex could not parse it (compound commands, pipelines):
				// recorded so a read model can say how much exploration
				// it cannot attribute to files.
				op = domain.ToolOpCommand
			}
			obs := domain.AgentToolObservation{
				Key:        codexObservationKey(source, "parsed_cmd", itemID, itoa(int64(i))),
				Ordinal:    offset,
				ObservedAt: observedAt,
				Origin:     domain.OriginAgentExploration,
				Op:         op,
				ToolName:   CodexParsedCommandTool,
			}
			switch {
			case op == domain.ToolOpCommand:
				obs.PathScope = domain.ToolPathNone
			case strings.TrimSpace(pc.Path) != "":
				obs.PathScope, obs.Path = scope.classify(pc.Path, base)
			case op != domain.ToolOpRead && base != "":
				// A search or listing with no path runs in its cwd.
				obs.PathScope, obs.Path = scope.classify(base, "")
			default:
				obs.PathScope = domain.ToolPathUnresolved
			}
			facts.Observations = append(facts.Observations, obs)
		}
	case "FileChange":
		paths := make([]string, 0, len(p.Item.Changes))
		for path := range p.Item.Changes {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		if len(paths) > maxPatchPathsPerEvent {
			paths = paths[:maxPatchPathsPerEvent]
		}
		for _, path := range paths {
			obs := domain.AgentToolObservation{
				Key:        codexObservationKey(source, "file_change", itemID, path),
				Ordinal:    offset,
				ObservedAt: observedAt,
				Origin:     domain.OriginAgentExploration,
				Op:         domain.ToolOpEdit,
				ToolName:   CodexFileChangeTool,
			}
			obs.PathScope, obs.Path = scope.classify(path, scope.baseFor(p.Item.Cwd))
			facts.Observations = append(facts.Observations, obs)
		}
	case "UserMessage":
		var size int64
		for _, c := range p.Item.Content {
			size += int64(len(c.Text))
		}
		facts.Observations = append(facts.Observations, domain.AgentToolObservation{
			Key:         codexObservationKey(source, "user_item", itemID),
			Ordinal:     offset,
			ObservedAt:  observedAt,
			Origin:      domain.OriginAOContext,
			Op:          domain.ToolOpPrompt,
			ToolName:    "user_message",
			PathScope:   domain.ToolPathNone,
			ResultBytes: &size,
		})
	}
}

// Tool names of Codex sub-observations: facts about a call already counted,
// never calls of their own.
const (
	CodexParsedCommandTool = "codex_parsed_cmd"
	CodexFileChangeTool    = "codex_file_change"
	CodexPatchApplyTool    = "patch_apply_end"
)
