package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	promconfig "github.com/prometheus/common/config"
)

// sessionCookieName is the cookie the TS4500's web container issues on a
// successful POST /v1/login and expects back on every subsequent request.
// R1.11.2 documents the login/logout handshake but not the cookie's name;
// this is what the hardware actually sends (verified 2026-08-03 against a
// real library).
const sessionCookieName = "JSESSIONID"

// sessionLoginPath and sessionLogoutPath are appended to the instance's base
// URL, which already ends in /web/api/v1 (see config's address handling), so
// they resolve to the two endpoints R1.11.2 names as the only non-GET
// requests this exporter is ever allowed to issue.
const (
	sessionLoginPath  = "/login"
	sessionLogoutPath = "/logout"
)

// loginRequest is the body POST /v1/login expects. The field names are the
// manual's own (`user`, not `username`).
type loginRequest struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// loginError is one element of the error array the library returns on a
// failed login. R1.11.2 documents this shape for the API's error responses
// generally; a wrong password produces
// [{"error_type":"login_failed","error_description":"User name or password is incorrect."}].
type loginError struct {
	ErrorType        string `json:"error_type"`
	ErrorDescription string `json:"error_description"`
}

// SessionTransport authenticates against the TS4500's RoE session endpoint
// and is the reason this exporter can talk to real hardware at all.
//
// **The library does not accept HTTP Basic authentication.** Verified
// 2026-08-03 against a real machine: a GET carrying an Authorization header
// is answered 401 with no WWW-Authenticate challenge, while POST /v1/login
// with the same credentials returns 200 and a JSESSIONID cookie that makes
// every subsequent GET succeed. Nothing in prometheus/common's
// HTTPClientConfig can express that handshake, which is why it lives here as
// a RoundTripper rather than as configuration.
//
// It wraps the transport built from http_client_config, so TLS, proxying and
// connection pooling are untouched: this only adds the cookie and the
// re-login. One SessionTransport per watched machine, installed by
// internal/instance's clientFor on the *http.Client every collector of that
// machine shares, which is what makes the eighteen collectors of one library
// share ONE session rather than opening eighteen. The library's session
// table is finite, so that sharing is a correctness property, not a saving.
//
// **A 401 is treated as "the session expired", not as "the credentials are
// wrong".** The library expires sessions on its own schedule and R1.11.2
// documents no lifetime, so the only workable strategy is to notice the 401,
// log in again, and retry once. Bad credentials surface as a failing LOGIN
// (400 with an error_description), never as an endless 401 loop: login is
// the only place this type decides whether the account works.
//
// **The 400-on-bad-password path is why success is read from the status
// rather than from the cookie.** The library issues a JSESSIONID on a FAILED
// login too, so a transport that treated the cookie's presence as proof of
// login would cheerfully store an unauthenticated session and retry against
// it forever.
type SessionTransport struct {
	next     http.RoundTripper
	baseURL  string
	user     string
	password string

	// mu guards cookie and gen. gen counts successful logins, so a request
	// that took a 401 can tell "nobody has logged in since I read the
	// cookie" (log in) from "another goroutine already refreshed it while I
	// was in flight" (just use theirs). The concurrency ceiling of 1 makes
	// the second case rare on this exporter, but the transport is shared by
	// every collector of a machine and correctness here should not depend
	// on a limiter that an operator can raise.
	mu     sync.Mutex
	cookie *http.Cookie
	gen    uint64
}

// NewSessionTransport wraps next so that every request it carries is
// authenticated by a RoE session. baseURL is the instance's own address, the
// same one collectors build their paths against, so /login and /logout
// resolve beside the endpoints being polled.
//
// It performs no I/O: the first login happens on the first request that
// needs one, so constructing a Handle never blocks on a machine that is down.
func NewSessionTransport(next http.RoundTripper, baseURL, user, password string) *SessionTransport {
	if next == nil {
		next = http.DefaultTransport
	}
	return &SessionTransport{
		next:     next,
		baseURL:  strings.TrimSuffix(baseURL, "/"),
		user:     user,
		password: password,
	}
}

// RoundTrip attaches the current session cookie, and on a 401 logs in once
// and retries. It satisfies http.RoundTripper's contract of not modifying
// the request it is given: every send works on a clone.
func (s *SessionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cookie, gen := s.snapshot()

	resp, err := s.send(req, cookie)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}

	// A 401 means the session is missing or has expired. Retrying is only
	// safe for a request whose body can be produced again; every request
	// this exporter issues is a bodiless GET, so this guard is for a future
	// caller rather than for today's.
	if req.Body != nil && req.GetBody == nil {
		return resp, nil
	}
	drain(resp)

	cookie, err = s.login(req.Context(), gen)
	if err != nil {
		return nil, err
	}
	return s.send(req, cookie)
}

// Logout ends the session, and is not optional. The library's session table
// is finite and R1.11.2 documents no idle eviction, so an exporter that
// restarted without logging out would leak one session per instance per
// restart until the machine refused to open another. internal/instance's
// shutdown path calls this once the pollers have stopped.
//
// Calling it with no session established is a no-op, so a shutdown before
// the first successful login is not an error.
func (s *SessionTransport) Logout(ctx context.Context) error {
	s.mu.Lock()
	cookie := s.cookie
	s.cookie = nil
	s.gen++
	s.mu.Unlock()

	if cookie == nil {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+sessionLogoutPath, nil)
	if err != nil {
		return fmt.Errorf("build logout request for %s: %w", s.baseURL, err)
	}
	req.AddCookie(cookie)

	resp, err := s.next.RoundTrip(req)
	if err != nil {
		return fmt.Errorf("log out of %s: %w", s.baseURL, err)
	}
	defer drain(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("log out of %s: unexpected status %s", s.baseURL, resp.Status)
	}
	return nil
}

// snapshot reads the session in force, together with the generation it
// belongs to, so a caller that later takes a 401 can tell whether anyone
// refreshed it in the meantime.
func (s *SessionTransport) snapshot() (*http.Cookie, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cookie, s.gen
}

// send clones req, attaches the session cookie if there is one, and hands it
// to the wrapped transport. The clone is what keeps RoundTrip compliant, and
// re-deriving the body through GetBody is what makes the retry above safe.
func (s *SessionTransport) send(req *http.Request, cookie *http.Cookie) (*http.Response, error) {
	r := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("rewind request body for %s: %w", req.URL.Path, err)
		}
		r.Body = body
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	return s.next.RoundTrip(r)
}

// login performs the POST /v1/login handshake and stores the session it
// returns. seen is the generation the caller last observed: if it no longer
// matches, another goroutine already logged in while this one was in flight
// and its session is used instead of opening a second one.
//
// It goes through s.next rather than through s, which would recurse: the
// login request is the one request that must never be retried on a 401.
func (s *SessionTransport) login(ctx context.Context, seen uint64) (*http.Cookie, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.gen != seen && s.cookie != nil {
		return s.cookie, nil
	}

	// gosec flags any marshaled field whose name matches a secret pattern.
	// Here that is the entire point: R1.11.2 defines POST /v1/login as taking
	// the password in its JSON body, so there is no shape of this request
	// that does not carry it. The transport it travels on is TLS, and the
	// credential never appears in a URL, a header or a log line.
	body, err := json.Marshal(loginRequest{User: s.user, Password: s.password}) //nolint:gosec // G117: the login body is defined by R1.11.2 to carry the password
	if err != nil {
		return nil, fmt.Errorf("build login body for %s: %w", s.baseURL, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+sessionLoginPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build login request for %s: %w", s.baseURL, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.next.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("log in to %s: %w", s.baseURL, err)
	}
	defer drain(resp)

	// Status, never the cookie: the library issues a JSESSIONID on a failed
	// login too (verified against real hardware), so reading success off the
	// Set-Cookie header would store an unauthenticated session and retry
	// against it forever.
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("log in to %s as %q: %s", s.baseURL, s.user, describeLoginFailure(resp))
	}

	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			s.cookie = c
			s.gen++
			return c, nil
		}
	}
	return nil, fmt.Errorf("log in to %s as %q: status %s but no %s cookie", s.baseURL, s.user, resp.Status, sessionCookieName)
}

// describeLoginFailure turns the library's own error body into the error
// text an operator reads, falling back to the status when the body is not
// the documented shape. Surfacing error_description is what makes a wrong
// password say so, rather than showing a bare 400 that reads like a bug in
// this exporter.
//
// The body is read here rather than by the caller because a failed login is
// the one response whose content this type interprets; drain closes it
// either way.
func describeLoginFailure(resp *http.Response) string {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil || len(b) == 0 {
		return "status " + resp.Status
	}
	var errs []loginError
	if err := json.Unmarshal(b, &errs); err != nil || len(errs) == 0 || errs[0].ErrorDescription == "" {
		return "status " + resp.Status
	}
	return fmt.Sprintf("status %s: %s", resp.Status, errs[0].ErrorDescription)
}

// drain consumes and closes a response body so the connection returns to the
// pool. Called on every response this file discards, which on the retry path
// is a response that would otherwise hold a connection open for the whole
// idle timeout.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
	_ = resp.Body.Close()
}

// SessionCredentials reads the RoE account out of an http_client_config's
// basic_auth block and reports whether one is configured at all.
//
// **basic_auth carries the credentials, but no Authorization header is ever
// sent.** That deserves the surprise it causes. The TS4500 accepts no HTTP
// authentication scheme, so there is nothing for a header to do; what it
// wants is those same two strings POSTed to /v1/login. Reusing basic_auth
// rather than inventing a section keeps password_file, its relative-path
// resolution and its secret handling exactly as prometheus/common already
// implements them, and keeps operators writing the block they would expect
// to write. The caller strips BasicAuth from the config before building the
// transport, so the credentials reach the login handshake and nothing else.
//
// A password_file takes precedence over an inline password, matching how
// prometheus/common resolves the same pair.
func SessionCredentials(hcfg *promconfig.HTTPClientConfig) (user, password string, err error) {
	if hcfg == nil || hcfg.BasicAuth == nil {
		return "", "", nil
	}
	ba := hcfg.BasicAuth

	user = ba.Username
	if ba.UsernameFile != "" {
		b, readErr := os.ReadFile(ba.UsernameFile)
		if readErr != nil {
			return "", "", fmt.Errorf("read basic_auth username_file: %w", readErr)
		}
		user = strings.TrimSpace(string(b))
	}

	password = string(ba.Password)
	if ba.PasswordFile != "" {
		b, readErr := os.ReadFile(ba.PasswordFile)
		if readErr != nil {
			return "", "", fmt.Errorf("read basic_auth password_file: %w", readErr)
		}
		password = strings.TrimSpace(string(b))
	}

	if user == "" {
		return "", "", nil
	}
	return user, password, nil
}
