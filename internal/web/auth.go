// Package web is the web UI (design §9.3, D8): `templ` pages rendered by the Go
// server, htmx for partial updates and SSE for the live transcript. Its handlers
// are thin: they call the same service layer as the JSON API.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Session is who a request is, as far as a handler needs to know. CSRF is the
// token every POST of the session must carry.
type Session struct {
	CSRF string
	// ID names the session among the signed-in devices (see Devices); it is not
	// the cookie and cannot be used to act as the session.
	ID string
}

// Auth is everything the UI needs of authentication, so that the passkeys of
// D45 replace the token without touching a handler.
type Auth interface {
	// Session returns the session of a request, if it has a valid one.
	Session(r *http.Request) (Session, bool)
	// SignIn checks the sign-in form of r and, if it is right, starts a session
	// and sets its cookie.
	SignIn(w http.ResponseWriter, r *http.Request) error
	// SignOut ends the session of r and clears its cookie.
	SignOut(w http.ResponseWriter, r *http.Request)
}

// Starter is an Auth that can begin a session for a request that was verified by
// other means, as the passkey sign-in does.
type Starter interface {
	// StartFor begins the session of the passkey the sign-in was made with, so a
	// revoked passkey takes its sessions with it.
	// It returns false, with no session, when sessions were ended after the
	// request was stamped (Server.stamp).
	StartFor(w http.ResponseWriter, r *http.Request, passkeyID string) bool
}

// Generations is an Auth that counts the times its sessions were ended, so a
// sign-in can note the count before it checks whether it may start a session and
// be refused if a sweep ran in between.
type Generations interface {
	Generation() uint64
}

// stamp notes the generation on the request, before whatever the sign-in checks.
func (s *Server) stamp(r *http.Request) *http.Request {
	if g, ok := s.opt.Auth.(Generations); ok {
		return r.WithContext(context.WithValue(r.Context(), generationKey{}, g.Generation()))
	}
	return r
}

// Device is a signed-in browser as the device list shows it.
type Device struct {
	ID       string
	Label    string // a phone, a tablet or a computer and its browser, from the User-Agent
	Since    time.Time
	LastSeen time.Time
	Current  bool
}

// Devices is an Auth that can list its sessions and end one, so a lost phone is
// signed out from the tablet (D35). The sessions are per device, not per token:
// the API token stays one secret for the host's CLI.
type Devices interface {
	Devices(r *http.Request) []Device
	// Revoke ends the session with the ID and reports whether there was one.
	Revoke(id string) bool
}

// Streams is an Auth whose sessions can be watched by a long-lived response, so a
// live stream does not outlive its session (sign-out, revoke, expiry).
type Streams interface {
	// Peek reports whether the session of r is still valid. It does not count as
	// use, so an open stream does not keep an idle session alive.
	Peek(r *http.Request) bool
	// Watch calls end when the session of r ends. The returned function stops
	// watching; it is safe to call after the session ended.
	Watch(r *http.Request, end func()) (stop func())
}

// Errors of signing in.
var (
	ErrBadCredentials = errors.New("that is not the token")
	ErrTooManyTries   = errors.New("too many failed tries: wait a minute")
)

const (
	cookieName = "whr_session"
	sessionTTL = 12 * time.Hour
	// A phone is the device a thief or a child picks up, so a session on one ends
	// after a short idle time (D35); a tablet or a computer keeps one longer.
	phoneIdle     = 15 * time.Minute
	desktopIdle   = 8 * time.Hour
	maxFailures   = 5
	failureWindow = time.Minute
)

// TokenAuth signs in with the API token: the human pastes it once, and a random
// session ID in a cookie stands in for it from then on. Sessions are kept in
// memory, so a restart signs everyone out.
type TokenAuth struct {
	digest [sha256.Size]byte
	now    func() time.Time

	mu       sync.Mutex
	sessions map[[sha256.Size]byte]tokenSession // by the hash of the cookie value
	watchers map[[sha256.Size]byte]map[uint64]func()
	nextW    uint64
	failures []time.Time
	// gen counts the calls of EndSessions. A sign-in notes it before it checks
	// whether it may start, and a session is started only if it is still the same:
	// one that checked before a sweep cannot start after it.
	gen uint64
	// httpsHost is the host name of the HTTPS forwarder (public_url): a request to
	// it is HTTPS; X-Forwarded-Proto is never believed.
	httpsHost     string
	secureCookies bool
	// onEnd hears every session that ends, whatever ended it.
	onEnd func(sessionID string)
}

type tokenSession struct {
	csrf     string
	expires  time.Time
	lastSeen time.Time
	since    time.Time
	idle     time.Duration
	label    string
	id       string
	passkey  string // the passkey the session was started with; empty for the API token
}

// NewTokenAuth returns the token authentication. now may be nil.
func NewTokenAuth(token []byte, now func() time.Time) (*TokenAuth, error) {
	if len(token) == 0 {
		return nil, errors.New("web: a token is required")
	}
	if now == nil {
		now = time.Now
	}
	return &TokenAuth{digest: sha256.Sum256(token), now: now, sessions: map[[sha256.Size]byte]tokenSession{}}, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("web: no randomness: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Session implements Auth.
func (a *TokenAuth) Session(r *http.Request) (Session, bool) {
	c, err := r.Cookie(a.cookieFor(r))
	if err != nil || c.Value == "" {
		return Session{}, false
	}
	key := sha256.Sum256([]byte(c.Value))
	now := a.now()
	a.mu.Lock()
	s, ok := a.sessions[key]
	if !ok || !now.Before(s.expires) || now.Sub(s.lastSeen) > s.idle {
		ends := a.dropLocked(key)
		a.mu.Unlock()
		runAll(ends)
		return Session{}, false
	}
	s.lastSeen = now
	a.sessions[key] = s
	a.mu.Unlock()
	return Session{CSRF: s.csrf, ID: s.id}, true
}

// SignIn implements Auth. The token is compared in constant time through its
// hash, a wrong one is slowed down by a limit on failures, and the cookie is
// HttpOnly, SameSite=Strict, and Secure when the request came over HTTPS: TLS, or
// the public_url host (SetPublicHost); X-Forwarded-Proto is never believed.
func (a *TokenAuth) SignIn(w http.ResponseWriter, r *http.Request) error {
	if err := r.ParseForm(); err != nil {
		return ErrBadCredentials
	}
	now := a.now()
	a.mu.Lock()
	recent := a.failures[:0]
	for _, t := range a.failures {
		if now.Sub(t) < failureWindow {
			recent = append(recent, t)
		}
	}
	a.failures = recent
	if len(a.failures) >= maxFailures {
		a.mu.Unlock()
		return ErrTooManyTries
	}
	a.mu.Unlock()

	got := sha256.Sum256([]byte(strings.TrimSpace(r.PostForm.Get("token"))))
	if subtle.ConstantTimeCompare(got[:], a.digest[:]) != 1 {
		a.mu.Lock()
		a.failures = append(a.failures, now)
		a.mu.Unlock()
		return ErrBadCredentials
	}
	if !a.Start(w, r) {
		return ErrSignInChanged
	}
	return nil
}

// generationKey carries the generation a sign-in noted (see Server.stamp).
type generationKey struct{}

// ErrSignInChanged means the sessions were ended while a sign-in ran, so it did
// not start one: it asks to try again.
var ErrSignInChanged = errors.New("sign-in changed while it ran: try again")

// Generation returns how many times sessions were ended so far.
func (a *TokenAuth) Generation() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gen
}

// Start begins a session for a request that has proved who it is by other means
// (a passkey): a random ID in a cookie that is HttpOnly, SameSite=Strict, without a
// Domain attribute (so only this host gets it) and Secure when the request came
// over HTTPS.
func (a *TokenAuth) Start(w http.ResponseWriter, r *http.Request) bool { return a.StartFor(w, r, "") }

// StartFor implements Starter: Start for a session that belongs to a passkey. It
// returns false, with no session, when sessions were ended after the request noted
// the generation: whatever it checked before may no longer hold.
func (a *TokenAuth) StartFor(w http.ResponseWriter, r *http.Request, passkeyID string) bool {
	id := randomHex(32)
	now := a.now()
	label, phone := deviceOf(r.UserAgent())
	idle := desktopIdle
	if phone {
		idle = phoneIdle
	}
	a.mu.Lock()
	if g, ok := r.Context().Value(generationKey{}).(uint64); ok && g != a.gen {
		a.mu.Unlock()
		return false
	}
	a.sessions[sha256.Sum256([]byte(id))] = tokenSession{
		csrf: randomHex(32), expires: now.Add(sessionTTL), since: now, lastSeen: now, idle: idle, label: label, id: randomHex(6), passkey: passkeyID,
	}
	a.mu.Unlock()
	//nolint:gosec // Secure is set when the request came over HTTPS: the forwarder terminates TLS (D29) and the loopback listener is plain HTTP
	http.SetCookie(w, &http.Cookie{
		Name: a.cookieFor(r), Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: a.isHTTPS(r), MaxAge: int(sessionTTL / time.Second),
	})
	return true
}

// SignOut implements Auth.
func (a *TokenAuth) SignOut(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(a.cookieFor(r)); err == nil {
		a.mu.Lock()
		ends := a.dropLocked(sha256.Sum256([]byte(c.Value)))
		a.mu.Unlock()
		runAll(ends)
	}
	//nolint:gosec // clearing the cookie, with the same attributes it was set with
	http.SetCookie(w, &http.Cookie{Name: a.cookieFor(r), Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: a.isHTTPS(r), MaxAge: -1})
}

// cookieFor is the session cookie's name for a request: over HTTPS it carries the
// __Host- prefix, so a browser takes it only when it is Secure, has no Domain and
// Path=/, and a cookie another port of the same host name sets under the plain name
// (a preview, D33) is not read as the session. Over plain loopback HTTP, where
// __Host- cannot work, it keeps the plain name.
func (a *TokenAuth) cookieFor(r *http.Request) string {
	if a.isHTTPS(r) {
		return "__Host-" + cookieName
	}
	return cookieName
}

// OnSessionEnd sets what hears every session that ends: a sign-out, a revoke, a
// sweep and an expiry. What a session opened (a preview, D33) ends with it.
func (a *TokenAuth) OnSessionEnd(f func(sessionID string)) {
	a.mu.Lock()
	a.onEnd = f
	a.mu.Unlock()
}

// Sweep ends the sessions that expired or sat idle past their limit, so what they
// opened (a preview, D33) ends with them even if nobody comes back to that cookie.
// Without it an idle or lost phone's session would end only when its cookie was
// next seen. whr serve calls it every minute.
func (a *TokenAuth) Sweep() {
	now := a.now()
	a.mu.Lock()
	var ends []func()
	for key, s := range a.sessions {
		if !now.Before(s.expires) || now.Sub(s.lastSeen) > s.idle {
			ends = append(ends, a.dropLocked(key)...)
		}
	}
	a.mu.Unlock()
	runAll(ends)
}

// SetPublicHost names the HTTPS forwarder's host from public_url (D29). A request
// is HTTPS when it came over TLS itself or by that name: the forwarder terminates
// TLS, and what the request says about its own scheme (X-Forwarded-Proto) is never
// believed. Without public_url, and on loopback, it is plain HTTP.
func (a *TokenAuth) SetPublicHost(host string) {
	a.mu.Lock()
	a.httpsHost = strings.ToLower(host)
	a.mu.Unlock()
}

// isHTTPS selects secure session cookies: always for a remote listener,
// otherwise for TLS or the configured HTTPS forwarder host.
func (a *TokenAuth) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	a.mu.Lock()
	host, secure := a.httpsHost, a.secureCookies
	a.mu.Unlock()
	if secure {
		return true
	}
	if host == "" {
		return false
	}
	h := r.Host
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.EqualFold(h, host)
}

// Devices implements the Devices interface: the live sessions, newest first, and
// the one of r marked.
func (a *TokenAuth) Devices(r *http.Request) []Device {
	var mine [sha256.Size]byte
	if c, err := r.Cookie(a.cookieFor(r)); err == nil {
		mine = sha256.Sum256([]byte(c.Value))
	}
	now := a.now()
	a.mu.Lock()
	var ends []func()
	defer func() { runAll(ends) }()
	defer a.mu.Unlock()
	var out []Device
	for key, s := range a.sessions {
		if !now.Before(s.expires) || now.Sub(s.lastSeen) > s.idle {
			ends = append(ends, a.dropLocked(key)...)
			continue
		}
		out = append(out, Device{ID: s.id, Label: s.label, Since: s.since, LastSeen: s.lastSeen, Current: key == mine})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.After(out[j].Since) })
	return out
}

// Revoke implements the Devices interface.
func (a *TokenAuth) Revoke(id string) bool {
	a.mu.Lock()
	for key, s := range a.sessions {
		if s.id == id {
			ends := a.dropLocked(key)
			a.mu.Unlock()
			runAll(ends)
			return true
		}
	}
	a.mu.Unlock()
	return false
}

// EndSessions ends every session whose passkey ID the function accepts (the empty
// ID is a session started with the API token), and with it the live streams of those
// sessions. The passkey service calls it when the first passkey is enrolled (the
// token-started sessions end) and when a passkey is revoked (its sessions end).
func (a *TokenAuth) EndSessions(match func(passkeyID string) bool) {
	a.mu.Lock()
	a.gen++
	var ends []func()
	for key, s := range a.sessions {
		if match(s.passkey) {
			ends = append(ends, a.dropLocked(key)...)
		}
	}
	a.mu.Unlock()
	runAll(ends)
}

// dropLocked deletes a session and returns what watches it, which the caller runs
// after it has released the lock.
func (a *TokenAuth) dropLocked(key [sha256.Size]byte) []func() {
	sess, had := a.sessions[key]
	delete(a.sessions, key)
	var ends []func()
	if had && a.onEnd != nil {
		id, f := sess.id, a.onEnd
		ends = append(ends, func() { f(id) })
	}
	for _, f := range a.watchers[key] {
		ends = append(ends, f)
	}
	delete(a.watchers, key)
	return ends
}

func runAll(fs []func()) {
	for _, f := range fs {
		f()
	}
}

// Peek implements Streams.
func (a *TokenAuth) Peek(r *http.Request) bool {
	c, err := r.Cookie(a.cookieFor(r))
	if err != nil || c.Value == "" {
		return false
	}
	key := sha256.Sum256([]byte(c.Value))
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[key]
	return ok && now.Before(s.expires) && now.Sub(s.lastSeen) <= s.idle
}

// Watch implements Streams.
func (a *TokenAuth) Watch(r *http.Request, end func()) func() {
	c, err := r.Cookie(a.cookieFor(r))
	if err != nil || c.Value == "" {
		end()
		return func() {}
	}
	key := sha256.Sum256([]byte(c.Value))
	a.mu.Lock()
	if _, ok := a.sessions[key]; !ok {
		a.mu.Unlock()
		end()
		return func() {}
	}
	if a.watchers == nil {
		a.watchers = map[[sha256.Size]byte]map[uint64]func(){}
	}
	if a.watchers[key] == nil {
		a.watchers[key] = map[uint64]func(){}
	}
	a.nextW++
	id := a.nextW
	a.watchers[key][id] = end
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		delete(a.watchers[key], id)
	}
}

// deviceOf names a browser from its User-Agent, in a few fixed words (the header
// is the client's, so none of it is kept), and says whether it is a phone.
func deviceOf(ua string) (label string, phone bool) {
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(ua, s) {
				return true
			}
		}
		return false
	}
	kind := "Computer"
	switch {
	case has("iPhone"), has("Android") && has("Mobile"):
		kind, phone = "Phone", true
	case has("iPad"), has("Android"):
		kind = "Tablet"
	case has("Macintosh"):
		kind = "Mac"
	case has("Windows"):
		kind = "Windows computer"
	case has("Linux"):
		kind = "Linux computer"
	}
	browser := "browser"
	switch {
	case has("Edg/"):
		browser = "Edge"
	case has("Firefox/", "FxiOS"):
		browser = "Firefox"
	case has("Chrome/", "CriOS"):
		browser = "Chrome"
	case has("Safari/"):
		browser = "Safari"
	}
	return kind + ", " + browser, phone
}

// RequireSecureCookies makes every session cookie Secure for a remote listener.
func (a *TokenAuth) RequireSecureCookies() {
	a.mu.Lock()
	a.secureCookies = true
	a.mu.Unlock()
}
