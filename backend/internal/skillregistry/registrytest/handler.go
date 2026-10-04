package registrytest

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
)

// handler.go -- the fixture's request routing.
//
// It answers the protocol exactly as protocol.go defines it, and it records
// every path so a test can assert what was NOT requested. "Search downloaded
// nothing" is a claim about an absence, and an absence is only provable if
// something was counting.

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.URL.Path)
	failures := s.failures
	requireToken, authHeader, registryID := s.requireToken, s.authHeader, s.registryID
	s.mu.Unlock()

	if failures.ServerError {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	if failures.Unauthorized || !s.authorized(r, requireToken, authHeader) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}

	apiVersion := skillregistry.ProtocolVersion
	if failures.WrongAPIVersion {
		apiVersion = "ao.registry/v99"
	}
	contentType := "application/json"
	if failures.HTMLContentType {
		contentType = "text/html; charset=utf-8"
	}

	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case path == "/v1/registry":
		writeJSON(w, map[string]string{
			"apiVersion": apiVersion, "registryId": registryID, "displayName": "Fixture",
		}, contentType)

	case path == "/v1/revocations":
		s.mu.Lock()
		revs := append([]skillregistry.Revocation(nil), s.revocations...)
		s.mu.Unlock()
		if revs == nil {
			revs = []skillregistry.Revocation{}
		}
		writeJSON(w, map[string]any{"apiVersion": apiVersion, "revocations": revs}, contentType)

	case path == "/v1/skills":
		s.serveSearch(w, r, apiVersion, contentType, failures)

	case strings.HasSuffix(path, "/artifact"):
		s.serveArtifact(w, r, strings.TrimSuffix(path, "/artifact"), failures)

	case strings.HasSuffix(path, "/versions"):
		s.serveVersions(w, strings.TrimSuffix(strings.TrimPrefix(path, "/v1/skills/"), "/versions"),
			apiVersion, contentType)

	case strings.Contains(path, "/versions/"):
		s.serveRelease(w, path, apiVersion, contentType)

	default:
		http.NotFound(w, r)
	}
}

// authorized checks the credential the way a private registry would.
func (s *Server) authorized(r *http.Request, requireToken, authHeader string) bool {
	if requireToken == "" {
		return true
	}
	if authHeader == "" {
		return r.Header.Get("Authorization") == "Bearer "+requireToken
	}
	return r.Header.Get(authHeader) == requireToken
}

func (s *Server) serveSearch(
	w http.ResponseWriter, r *http.Request, apiVersion, contentType string, failures Failures,
) {
	query := r.URL.Query()
	includeRevoked := query.Get("includeRevoked") == "true"
	includeDeprecated := query.Get("includeDeprecated") == "true"
	text := strings.ToLower(strings.TrimSpace(query.Get("q")))
	limit := 100
	if raw := query.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	out := make([]skillregistry.Release, 0)
	for _, e := range s.entries() {
		rel := e.Release
		if rel.Revoked && !includeRevoked {
			continue
		}
		if rel.Deprecated && !includeDeprecated {
			continue
		}
		if text != "" && !strings.Contains(strings.ToLower(rel.SkillID+" "+rel.Name+" "+rel.Description), text) {
			continue
		}
		if len(out) >= limit {
			break
		}
		out = append(out, rel)
	}
	body := map[string]any{"apiVersion": apiVersion, "releases": out}
	if failures.OversizedMetadata {
		// A registry answering a search with megabytes of padding. The
		// ceiling, not the parser, is what has to stop it.
		body["releases"] = out
		s.writeOversized(w, contentType)
		return
	}
	if !failures.NoETag {
		w.Header().Set("ETag", `"fixture-search"`)
	}
	if match := r.Header.Get("If-None-Match"); match == `"fixture-search"` && !failures.NoETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, body, contentType)
}

// writeOversized streams past the metadata ceiling without building the whole
// body in memory.
func (s *Server) writeOversized(w http.ResponseWriter, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	chunk := strings.Repeat("A", 1<<20)
	for i := 0; i < 12; i++ {
		if _, err := w.Write([]byte(chunk)); err != nil {
			return
		}
	}
}

func (s *Server) serveVersions(w http.ResponseWriter, skillID, apiVersion, contentType string) {
	out := make([]skillregistry.Release, 0)
	for _, e := range s.entries() {
		if e.Release.SkillID == skillID {
			out = append(out, e.Release)
		}
	}
	if len(out) == 0 {
		http.NotFound(w, &http.Request{})
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"apiVersion": apiVersion, "releases": out}, contentType)
}

func (s *Server) serveRelease(w http.ResponseWriter, path, apiVersion, contentType string) {
	skillID, version, ok := splitReleasePath(path)
	if !ok {
		http.NotFound(w, &http.Request{})
		return
	}
	entry, found := s.lookup(skillID, version)
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"apiVersion": apiVersion, "release": entry.Release}, contentType)
}

func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request, path string, failures Failures) {
	if failures.RedirectArtifactTo != "" {
		http.Redirect(w, r, failures.RedirectArtifactTo, http.StatusFound)
		return
	}
	skillID, version, ok := splitReleasePath(path)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	entry, found := s.lookup(skillID, version)
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", skillregistry.ArtifactMediaType)
	switch {
	case failures.ArtifactBomb:
		// A small gzip that expands past the uncompressed budget. This is the
		// case a limit on the COMPRESSED body does not catch.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bombArchive())
	case failures.OversizedArtifact:
		w.WriteHeader(http.StatusOK)
		chunk := make([]byte, 1<<20)
		for i := 0; i < 80; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	default:
		body := tarGz(entry.Files, failures.ArtifactSymlink, failures.ArtifactTraversal)
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// splitReleasePath reads "/v1/skills/{id}/versions/{version}".
func splitReleasePath(path string) (string, string, bool) {
	rest, ok := strings.CutPrefix(path, "/v1/skills/")
	if !ok {
		return "", "", false
	}
	skillID, version, ok := strings.Cut(rest, "/versions/")
	if !ok || skillID == "" || version == "" {
		return "", "", false
	}
	return skillID, version, true
}
