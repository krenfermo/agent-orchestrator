// Command fixture is a deliberately-vulnerable LOCAL web target for the
// active-pentest E2E. It exists only to be tested against inside a Docker
// network the test creates and destroys. It is NEVER a real service, is bound
// to no public interface by anything but the test, and ships no real secret —
// the "secret" it leaks is a well-known example value.
//
// It is intentionally misconfigured so the checker has something true to find:
//   - no security headers (CSP, X-Content-Type-Options, X-Frame-Options, ...)
//   - a session cookie with no Secure/HttpOnly/SameSite flags
//   - CORS that reflects any Origin and allows credentials
//   - a Server banner that discloses a version
//   - an exposed /.git/config
//   - a /redirect that sends the client off the authorized target
//   - a body carrying an example AWS key, so the daemon's redactor has
//     something to redact when the checker quotes it
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	mux := http.NewServeMux()

	mux.HandleFunc("/.git/config", func(w http.ResponseWriter, r *http.Request) {
		// A path that must never be web-readable. The body is dummy.
		_, _ = w.Write([]byte("[core]\n\trepositoryformatversion = 0\n\tbare = false\n"))
	})

	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		// A redirect OFF the authorized target. The checker must not follow it,
		// and if it did, the proxy would refuse the off-allowlist host.
		http.Redirect(w, r, "http://evil.example/stolen", http.StatusFound)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "nginx/1.18.0")
		if origin := r.Header.Get("Origin"); origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		// A flagless session cookie whose value is a fake secret: the checker
		// quotes the Set-Cookie in its finding, and the daemon redacts it.
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "AKIAIOSFODNN7EXAMPLE"})
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("<html><body>vulnerable fixture ok</body></html>"))
	})

	fmt.Fprintf(os.Stderr, "fixture: listening on %s\n", *addr)
	srv := &http.Server{Addr: *addr, Handler: mux}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "fixture: %v\n", err)
		os.Exit(1)
	}
}
