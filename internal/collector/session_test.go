package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	promconfig "github.com/prometheus/common/config"
)

// fakeLibrary is an httptest handler that behaves like a real TS4500's RoE
// API: it refuses every request that does not carry a session cookie it
// issued, and it issues one only from POST /login with the right
// credentials.
//
// This is the piece the rest of the suite was missing. Every other collector
// test in this package points at a server that answers 200 to anything, so
// all 306 of them passed against an exporter that could not authenticate
// against real hardware at all. A fake that actually refuses is what turns
// that class of bug into a test failure.
type fakeLibrary struct {
	user, password string

	mu       sync.Mutex
	sessions map[string]bool
	next     int

	logins  atomic.Int64
	logouts atomic.Int64
	gets    atomic.Int64

	// expireAfter, when > 0, invalidates each issued session after that many
	// successful GETs, which is how the expiry-and-relogin path is driven.
	expireAfter int
	served      atomic.Int64
}

func newFakeLibrary(user, password string) *fakeLibrary {
	return &fakeLibrary{user: user, password: password, sessions: map[string]bool{}}
}

func (f *fakeLibrary) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, sessionLoginPath):
		f.handleLogin(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, sessionLogoutPath):
		f.handleLogout(w, r)
	default:
		f.handleGet(w, r)
	}
}

func (f *fakeLibrary) handleLogin(w http.ResponseWriter, r *http.Request) {
	f.logins.Add(1)
	var body loginRequest
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	f.next++
	id := "SESSION" + string(rune('A'+f.next-1))
	f.mu.Unlock()

	// The real library sets a cookie even when the login FAILS, which is the
	// exact behaviour that makes reading success off the cookie wrong.
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: id, Path: "/"})

	if body.User != f.user || body.Password != f.password {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`[{"error_type":"login_failed","error_description":"User name or password is incorrect."}]`))
		return
	}

	f.mu.Lock()
	f.sessions[id] = true
	f.mu.Unlock()
	f.served.Store(0)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeLibrary) handleLogout(w http.ResponseWriter, r *http.Request) {
	f.logouts.Add(1)
	if c, err := r.Cookie(sessionCookieName); err == nil {
		f.mu.Lock()
		delete(f.sessions, c.Value)
		f.mu.Unlock()
	}
	w.WriteHeader(http.StatusOK)
}

func (f *fakeLibrary) handleGet(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		// No WWW-Authenticate, exactly as the real library answers.
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	ok := f.sessions[c.Value]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.expireAfter > 0 && f.served.Add(1) > int64(f.expireAfter) {
		f.mu.Lock()
		delete(f.sessions, c.Value)
		f.mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.gets.Add(1)
	_, _ = w.Write([]byte(`[{"ok":true}]`))
}

// TestFetchWithoutASessionIsRefused is the regression test for the gap this
// file closes: a Client built the way every collector's own test builds one
// cannot talk to a library that requires the handshake. It fails with 401
// rather than hanging or panicking, and it is what the rest of this file
// proves SessionTransport fixes.
func TestFetchWithoutASessionIsRefused(t *testing.T) {
	srv := httptest.NewServer(newFakeLibrary("monitor", "secret"))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	if _, err := c.Fetch(context.Background(), "/library"); err == nil {
		t.Fatal("Fetch against a session-requiring library succeeded, want a 401 error")
	} else if !strings.Contains(err.Error(), "401") {
		t.Fatalf("Fetch error = %v, want it to name the 401", err)
	}
}

// TestSessionTransport_LogsInOnceAndReusesTheSession is the core behaviour:
// the first request triggers exactly one login, and every request after it
// rides the same session rather than opening another. The library's session
// table is finite, so "once" is the assertion that matters.
func TestSessionTransport_LogsInOnceAndReusesTheSession(t *testing.T) {
	lib := newFakeLibrary("monitor", "secret")
	srv := httptest.NewServer(lib)
	defer srv.Close()

	c := sessionClient(t, srv.URL, "monitor", "secret")

	for i := 0; i < 5; i++ {
		if _, err := c.Fetch(context.Background(), "/library"); err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
	}
	if got := lib.logins.Load(); got != 1 {
		t.Errorf("logins = %d, want 1: the session must be reused across requests", got)
	}
	if got := lib.gets.Load(); got != 5 {
		t.Errorf("successful GETs = %d, want 5", got)
	}
}

// TestSessionTransport_RelogsInWhenTheSessionExpires drives the path the
// library will actually exercise in production: R1.11.2 documents no session
// lifetime, so the exporter has to notice the 401, log in again and retry.
// The caller must see none of it.
func TestSessionTransport_RelogsInWhenTheSessionExpires(t *testing.T) {
	lib := newFakeLibrary("monitor", "secret")
	lib.expireAfter = 2
	srv := httptest.NewServer(lib)
	defer srv.Close()

	c := sessionClient(t, srv.URL, "monitor", "secret")

	for i := 0; i < 6; i++ {
		if _, err := c.Fetch(context.Background(), "/library"); err != nil {
			t.Fatalf("Fetch %d: %v (an expired session must be renewed transparently)", i, err)
		}
	}
	// 6 requests with a session good for 2 each: the first login plus one
	// renewal per expiry.
	if got := lib.logins.Load(); got < 2 {
		t.Errorf("logins = %d, want >= 2: an expired session must trigger a re-login", got)
	}
}

// TestSessionTransport_BadCredentialsFailLoudly pins the behaviour that
// separates a wrong password from an expired session. The library answers a
// failed login with 400 AND a cookie, so this also proves the transport does
// not store that cookie and retry against it forever.
func TestSessionTransport_BadCredentialsFailLoudly(t *testing.T) {
	lib := newFakeLibrary("monitor", "secret")
	srv := httptest.NewServer(lib)
	defer srv.Close()

	c := sessionClient(t, srv.URL, "monitor", "wrong-password")

	_, err := c.Fetch(context.Background(), "/library")
	if err == nil {
		t.Fatal("Fetch with bad credentials succeeded, want an error")
	}
	// The library's own error_description must reach the operator: a bare
	// 400 reads like a bug in this exporter rather than a wrong password.
	if !strings.Contains(err.Error(), "User name or password is incorrect") {
		t.Errorf("error = %v, want it to carry the library's error_description", err)
	}

	// One login attempt per request, never a loop: two Fetches must produce
	// exactly two logins, not an unbounded retry storm.
	before := lib.logins.Load()
	_, _ = c.Fetch(context.Background(), "/library")
	if got := lib.logins.Load() - before; got != 1 {
		t.Errorf("logins for one Fetch = %d, want 1: a failed login must not be retried in a loop", got)
	}
}

// TestSessionTransport_LogoutEndsTheSession covers the shutdown path. A
// leaked session is not cosmetic: the library's session table is finite, so
// an exporter that restarts without logging out eventually cannot log in.
func TestSessionTransport_LogoutEndsTheSession(t *testing.T) {
	lib := newFakeLibrary("monitor", "secret")
	srv := httptest.NewServer(lib)
	defer srv.Close()

	st := NewSessionTransport(http.DefaultTransport, srv.URL, "monitor", "secret")
	c := NewClientFor(srv.URL, &http.Client{Transport: st, Timeout: time.Second})

	if _, err := c.Fetch(context.Background(), "/library"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := st.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if got := lib.logouts.Load(); got != 1 {
		t.Fatalf("logouts = %d, want 1", got)
	}

	// The session really is gone server-side: the next request has to log in
	// again rather than reuse a cookie the library has already discarded.
	before := lib.logins.Load()
	if _, err := c.Fetch(context.Background(), "/library"); err != nil {
		t.Fatalf("Fetch after logout: %v", err)
	}
	if got := lib.logins.Load(); got != before+1 {
		t.Errorf("logins after logout = %d, want %d: logout must clear the stored session", got, before+1)
	}
}

// TestSessionTransport_LogoutWithoutASessionIsANoOp: shutting down before
// the first successful login is normal (a library that was down the whole
// time), not an error to report.
func TestSessionTransport_LogoutWithoutASessionIsANoOp(t *testing.T) {
	lib := newFakeLibrary("monitor", "secret")
	srv := httptest.NewServer(lib)
	defer srv.Close()

	st := NewSessionTransport(http.DefaultTransport, srv.URL, "monitor", "secret")
	if err := st.Logout(context.Background()); err != nil {
		t.Fatalf("Logout with no session = %v, want nil", err)
	}
	if got := lib.logouts.Load(); got != 0 {
		t.Errorf("logouts = %d, want 0: nothing to end means nothing to send", got)
	}
}

// TestSessionTransport_ConcurrentFirstRequestsLogInOnce covers the seam the
// generation counter exists for. The concurrency ceiling is 1 per instance
// today, so this is rare in production, but the transport is shared by every
// collector of a machine and an operator can raise that ceiling.
func TestSessionTransport_ConcurrentFirstRequestsLogInOnce(t *testing.T) {
	lib := newFakeLibrary("monitor", "secret")
	srv := httptest.NewServer(lib)
	defer srv.Close()

	c := sessionClient(t, srv.URL, "monitor", "secret")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Fetch(context.Background(), "/library")
		}()
	}
	wg.Wait()

	// Eight goroutines racing on an empty session must not open eight
	// sessions on a machine whose session table is finite.
	if got := lib.logins.Load(); got > 2 {
		t.Errorf("logins = %d, want at most 2 for 8 concurrent first requests", got)
	}
}

// TestSessionTransport_DoesNotMutateTheRequest pins http.RoundTripper's own
// contract: RoundTrip must not modify the request it is handed. Adding the
// cookie to the caller's request rather than to a clone would leak a stale
// session into any retry the caller performs itself.
func TestSessionTransport_DoesNotMutateTheRequest(t *testing.T) {
	lib := newFakeLibrary("monitor", "secret")
	srv := httptest.NewServer(lib)
	defer srv.Close()

	st := NewSessionTransport(http.DefaultTransport, srv.URL, "monitor", "secret")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/library", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := st.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	drain(resp)

	if got := req.Header.Get("Cookie"); got != "" {
		t.Errorf("caller's request carries Cookie %q after RoundTrip, want it untouched", got)
	}
}

// TestSessionCredentials covers the credential source, including the
// surprise it deliberately carries: basic_auth is where the RoE account is
// written, and no Authorization header is ever produced from it.
func TestSessionCredentials(t *testing.T) {
	t.Run("no config and no basic_auth yield no credentials", func(t *testing.T) {
		for _, cfg := range []*promconfig.HTTPClientConfig{nil, {}} {
			u, p, err := SessionCredentials(cfg)
			if err != nil {
				t.Fatalf("SessionCredentials: %v", err)
			}
			if u != "" || p != "" {
				t.Errorf("got %q/%q, want empty: no basic_auth means no session auth", u, p)
			}
		}
	})

	t.Run("an inline password is read", func(t *testing.T) {
		u, p, err := SessionCredentials(&promconfig.HTTPClientConfig{
			BasicAuth: &promconfig.BasicAuth{Username: "monitor", Password: "secret"},
		})
		if err != nil {
			t.Fatalf("SessionCredentials: %v", err)
		}
		if u != "monitor" || p != "secret" {
			t.Errorf("got %q/%q, want monitor/secret", u, p)
		}
	})

	t.Run("password_file wins over an inline password and is trimmed", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "pw")
		// Trailing newline is what an editor leaves behind, and sending it
		// as part of the password would fail the login for a reason nobody
		// could see.
		if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		u, p, err := SessionCredentials(&promconfig.HTTPClientConfig{
			BasicAuth: &promconfig.BasicAuth{Username: "monitor", Password: "inline", PasswordFile: path},
		})
		if err != nil {
			t.Fatalf("SessionCredentials: %v", err)
		}
		if u != "monitor" || p != "from-file" {
			t.Errorf("got %q/%q, want monitor/from-file", u, p)
		}
	})

	t.Run("an unreadable password_file is an error, not an empty password", func(t *testing.T) {
		_, _, err := SessionCredentials(&promconfig.HTTPClientConfig{
			BasicAuth: &promconfig.BasicAuth{Username: "monitor", PasswordFile: "/nonexistent/pw"},
		})
		if err == nil {
			t.Error("SessionCredentials with an unreadable password_file returned nil error")
		}
	})
}

// sessionClient builds the Client shape internal/instance installs: one
// *http.Client per machine whose transport carries the session, with the
// per-request deadline on the Client itself.
func sessionClient(t *testing.T, target, user, password string) *Client {
	t.Helper()
	st := NewSessionTransport(http.DefaultTransport, target, user, password)
	return NewClientFor(target, &http.Client{Transport: st, Timeout: 2 * time.Second})
}
