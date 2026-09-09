// Command ao-egress-proxy is the only route out of a skill container.
//
// It reads one policy file and serves a forward proxy that enforces it. It has
// no flags that widen the allowlist, no API to reconfigure it, and no source of
// authority other than the file AO wrote before the container started — so
// there is nothing inside the container that can talk it into allowing more.
//
// It runs as a sidecar on AO's internal network, dual-homed onto a second
// network with egress. The skill container joins ONLY the internal one, where
// (measured) an internet IP is unreachable, the cloud-metadata address is
// unreachable, and the embedded resolver answers SERVFAIL for external names.
// So this process is not a politeness the workload could decline; it is the
// only path that exists.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillegress"
)

func main() {
	policyPath := flag.String("policy", "", "path to the JSON policy this proxy enforces")
	addr := flag.String("addr", ":3128", "listen address")
	decisionsPath := flag.String("decisions", "", "path to append authorization decisions to")
	flag.Parse()

	if *policyPath == "" {
		log.Fatal("ao-egress-proxy: -policy is required; a proxy with no policy allows nothing " +
			"and should not start")
	}
	body, err := os.ReadFile(*policyPath)
	if err != nil {
		log.Fatalf("ao-egress-proxy: read policy: %v", err)
	}
	policy, err := skillegress.DecodePolicy(body)
	if err != nil {
		// A tampered or malformed policy is a refusal to start, never a
		// fallback to a permissive default.
		log.Fatalf("ao-egress-proxy: %v", err)
	}

	recorder := skillegress.Recorder(&skillegress.MemoryRecorder{})
	if *decisionsPath != "" {
		fileRecorder, openErr := newFileRecorder(*decisionsPath)
		if openErr != nil {
			// No defer to skip: nothing is open yet. A proxy that cannot
			// record its decisions does not start, because the decisions ARE
			// the evidence the control is enforced.
			log.Fatalf("ao-egress-proxy: open decisions file: %v", openErr)
		}
		recorder = fileRecorder
	}

	proxy := skillegress.NewProxy(policy, recorder)
	server := &http.Server{
		Addr:              *addr,
		Handler:           proxy,
		ReadHeaderTimeout: 15 * time.Second,
	}
	log.Printf("ao-egress-proxy: enforcing %s until %s", policy.Summary(), policy.ExpiresAt.Format(time.RFC3339))
	serveErr := server.ListenAndServe()
	// Close the recorder before exiting rather than leaving it to a deferred
	// call that log.Fatalf would skip: the decisions are the evidence that the
	// control was enforced, and losing the tail of them loses the proof.
	if closer, ok := recorder.(interface{ Close() }); ok {
		closer.Close()
	}
	if serveErr != nil {
		log.Printf("ao-egress-proxy: %v", serveErr)
		os.Exit(1)
	}
}

// fileRecorder appends one JSON decision per line. Decisions carry
// destinations and outcomes; they never carry payloads or credentials.
type fileRecorder struct {
	mu sync.Mutex
	f  *os.File
}

func newFileRecorder(path string) (*fileRecorder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &fileRecorder{f: f}, nil
}

func (r *fileRecorder) Record(d skillegress.Decision) {
	body, err := json.Marshal(d)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = fmt.Fprintf(r.f, "%s\n", body)
}

func (r *fileRecorder) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.f.Close()
}
