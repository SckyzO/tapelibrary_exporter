package instance

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	promconfig "github.com/prometheus/common/config"

	"github.com/sckyzo/tapelibrary_exporter/internal/collector"
)

// TestClientForInstallsTheSession covers the seam that makes this exporter
// able to talk to real hardware: basic_auth's credentials must reach the RoE
// login handshake, and must NOT also be sent as an Authorization header.
//
// The header assertion is the one that would silently regress. Leaving
// BasicAuth in the config handed to NewHTTPClient still produces a working
// exporter — the library ignores the header — while putting the monitoring
// account's password on every single request for no purpose.
func TestClientForInstallsTheSession(t *testing.T) {
	var sawAuthHeader atomic.Bool
	var logins atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuthHeader.Store(true)
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/login") {
			logins.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "S1", Path: "/"})
			w.WriteHeader(http.StatusOK)
			return
		}
		if _, err := r.Cookie("JSESSIONID"); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"ok":true}]`))
	}))
	defer srv.Close()

	cfg := &promconfig.HTTPClientConfig{
		BasicAuth: &promconfig.BasicAuth{Username: "monitor", Password: "secret"},
	}
	hc, session, err := clientFor(cfg, srv.URL)
	if err != nil {
		t.Fatalf("clientFor: %v", err)
	}
	if session == nil {
		t.Fatal("clientFor returned a nil session for a config carrying basic_auth")
	}

	// The caller's config must not have been mutated: clientFor copies it.
	if cfg.BasicAuth == nil {
		t.Error("clientFor cleared BasicAuth on the CALLER's config, not on its own copy")
	}

	h := NewHandle("lib1", srv.URL, hc, 0, nil, 0)
	h.session = session
	c, err := h.ClientFor(2e9) // 2s
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}

	if _, err := c.Fetch(t.Context(), "/library"); err != nil {
		t.Fatalf("Fetch through the session transport: %v", err)
	}
	if logins.Load() != 1 {
		t.Errorf("logins = %d, want 1", logins.Load())
	}
	if sawAuthHeader.Load() {
		t.Error("an Authorization header reached the server: basic_auth must be stripped, its credentials go to /login instead")
	}

	// Logout is what keeps the library's finite session table from filling up
	// across restarts, so the shutdown path has to actually reach it.
	if err := session.Logout(t.Context()); err != nil {
		t.Errorf("Logout: %v", err)
	}
}

// TestClientForWithoutCredentialsInstallsNoSession pins the no-authentication
// path: with no basic_auth there is no handshake and no wrapper, so a target
// that needs no credentials behaves exactly as it did before sessions
// existed. Every other test in this package relies on that.
func TestClientForWithoutCredentialsInstallsNoSession(t *testing.T) {
	for name, cfg := range map[string]*promconfig.HTTPClientConfig{
		"nil config":     nil,
		"no basic_auth":  {},
		"empty username": {BasicAuth: &promconfig.BasicAuth{Password: "secret"}},
	} {
		t.Run(name, func(t *testing.T) {
			hc, session, err := clientFor(cfg, "https://example.invalid")
			if err != nil {
				t.Fatalf("clientFor: %v", err)
			}
			if session != nil {
				t.Error("clientFor built a session with no username configured")
			}
			if hc == nil {
				t.Fatal("clientFor returned a nil client")
			}
			if _, isSession := hc.Transport.(*collector.SessionTransport); isSession {
				t.Error("the transport was wrapped despite no credentials being configured")
			}
		})
	}
}
