package web

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/wstein/workharbor/internal/api"
	"github.com/wstein/workharbor/internal/domain"
	"github.com/wstein/workharbor/internal/store"
)

//go:embed static
var staticFS embed.FS

// Options configures a Server.
type Options struct {
	// Auth decides who may use the UI. Required.
	Auth Auth
	// Store keeps the idempotency keys of the forms. Required.
	Store *store.Store
	// Heartbeat is the interval of the live transcript's keep-alive comments.
	// Default 15 s.
	Heartbeat time.Duration
	// Now is the clock of an answer. Default time.Now.
	Now func() time.Time
	// Passkeys, if set, turns on the passkey enrolment page, the passkey sign-in and
	// the step-up approvals (D45). Without it the UI signs in with the token and
	// refuses to answer a review.
	Passkeys Passkeys
	// RequirePasskey latches remote sign-in to passkeys even after the last key
	// is revoked. Only trusted listener configuration sets this value.
	RequirePasskey bool
	// Changes, if set, adds the page of changes that need a passkey: a workflow
	// change the host has not confirmed and the revocation of the forge tokens
	// (D45, issue #107). Without Passkeys it only lists them.
	Changes Changes
	// Setup adds read-only diagnostics; nil disables the setup routes.
	Setup Setup
	// SetupOrigin is the configured browser origin, including through an HTTPS
	// forwarder. Empty uses the request's direct origin.
	SetupOrigin string
	// Previews, if set, adds the previews of an agent's dev servers to the task
	// view and the inbox (D33). Without it the UI shows none.
	Previews api.Previews
	// OnError hears an internal error, which the browser only sees as a generic
	// message. Optional.
	OnError func(error)
}

// Server is the web UI. It calls the same Backend as the JSON API.
type Server struct {
	be    api.Backend
	opt   Options
	keys  onceKeys
	setup setupState
}

// New returns a server.
func New(be api.Backend, opt Options) (*Server, error) {
	if opt.Auth == nil {
		return nil, errors.New("web: an authentication is required")
	}
	if opt.Store == nil {
		return nil, errors.New("web: a store is required for idempotency keys")
	}
	if opt.Heartbeat <= 0 {
		opt.Heartbeat = 15 * time.Second
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Server{be: be, opt: opt}, nil
}

// csp forbids everything the pages do not use: no inline script or style, no
// other origin, no framing.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; manifest-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// Handler returns the UI as an http.Handler. Everything but the sign-in page and
// the static files needs a session, the live stream included, and every POST
// needs the session's CSRF token and a same-site origin.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	files := http.StripPrefix("/static/", http.FileServerFS(static))
	mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		files.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /manifest.webmanifest", s.staticAt("manifest.webmanifest", "application/manifest+json"))
	mux.HandleFunc("GET /sw.js", s.staticAt("sw.js", "text/javascript"))
	mux.HandleFunc("GET /offline", s.staticAt("offline.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /devices", s.authed(false, s.devices))
	mux.HandleFunc("POST /devices/{device}/revoke", s.authed(true, s.revokeDevice))
	mux.HandleFunc("GET /enrol", s.enrolPage)
	mux.HandleFunc("POST /passkey/enrol/begin", s.enrolBegin)
	mux.HandleFunc("POST /passkey/enrol/finish", s.enrolFinish)
	mux.HandleFunc("POST /passkey/login/begin", s.passkeyLoginBegin)
	mux.HandleFunc("POST /passkey/login/finish", s.passkeyLoginFinish)
	mux.HandleFunc("POST /decisions/{decision}/stepup/begin", s.authedJSON(s.stepUpBegin))
	mux.HandleFunc("POST /decisions/{decision}/stepup/finish", s.authedJSON(s.stepUpFinish))
	mux.HandleFunc("GET /console/ssh", s.authed(false, s.consoleCertificatePage))
	mux.HandleFunc("POST /console/ssh/begin", s.authedJSON(s.consoleCertificateBegin))
	mux.HandleFunc("POST /console/ssh/finish", s.authedJSON(s.consoleCertificateFinish))
	mux.HandleFunc("GET /setup", s.authed(false, s.setupPage))
	mux.HandleFunc("POST /setup/check", s.authed(true, s.setupCheck))
	mux.HandleFunc("GET /setup/report.json", s.authed(false, s.setupReport))
	mux.HandleFunc("GET /changes", s.authed(false, s.changes))
	mux.HandleFunc("POST /changes/{change}/stepup/begin", s.authedJSON(s.changeBegin))
	mux.HandleFunc("POST /changes/{change}/stepup/finish", s.authedJSON(s.changeFinish))
	mux.HandleFunc("POST /secrets/revoke-tokens/stepup/begin", s.authedJSON(s.revokeBegin))
	mux.HandleFunc("POST /secrets/revoke-tokens/stepup/finish", s.authedJSON(s.revokeFinish))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.authed(true, s.logout))
	mux.HandleFunc("GET /{$}", s.authed(false, s.harbor))
	mux.HandleFunc("GET /inbox", s.authed(false, s.inbox))
	mux.HandleFunc("POST /tasks", s.authed(true, s.start))
	mux.HandleFunc("GET /tasks/{task}", s.authed(false, s.task))
	mux.HandleFunc("GET /tasks/{task}/events", s.authed(false, s.events))
	mux.HandleFunc("POST /tasks/{task}/say", s.authed(true, s.say))
	mux.HandleFunc("GET /tasks/{task}/cancel", s.authed(false, s.cancelPage))
	mux.HandleFunc("POST /tasks/{task}/cancel", s.authed(true, s.cancel))
	mux.HandleFunc("POST /tasks/{task}/pause", s.authed(true, s.pause))
	mux.HandleFunc("POST /tasks/{task}/resume", s.authed(true, s.resume))
	mux.HandleFunc("GET /tasks/{task}/purge", s.authed(false, s.purgePage))
	mux.HandleFunc("POST /tasks/{task}/purge", s.authed(true, s.purge))
	mux.HandleFunc("POST /tasks/{task}/open", s.authed(true, s.open))
	mux.HandleFunc("POST /decisions/{decision}/answer", s.authed(true, s.answer))
	mux.HandleFunc("POST /tasks/{task}/previews", s.authed(true, s.startPreview))
	mux.HandleFunc("GET /previews/{preview}/go", s.authed(false, s.goPreview))
	mux.HandleFunc("POST /previews/{preview}/close", s.authed(true, s.closePreview))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		if _, pattern := mux.Handler(r); pattern == "" {
			s.fail(w, r, Session{}, &domain.NotFoundError{Kind: "page", ID: r.URL.Path})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// staticAt serves one embedded file at a path outside /static, for the PWA's
// manifest, service worker and offline page. They hold nothing private, and the
// browser fetches them without a session, so they are public. The service worker
// is never cached by the browser's HTTP cache, so an update reaches the device.
func (s *Server) staticAt(name, contentType string) http.HandlerFunc {
	body, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		panic("web: missing embedded file " + name)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(body)
	}
}

// authed wraps a handler that needs a session. With write set it is a POST and
// the CSRF token and the origin are checked before the handler runs. A request
// without a session goes to the sign-in page, or gets a 401 when it is the
// stream.
func (s *Server) authed(write bool, h func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.opt.Auth.Session(r)
		if !ok {
			if strings.HasSuffix(r.URL.Path, "/events") {
				http.Error(w, "sign in first", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if write {
			if err := s.checkWrite(r, sess); err != nil {
				s.fail(w, r, sess, err)
				return
			}
		}
		h(w, r, sess)
	}
}

// authedJSON wraps a handler of the page's script: it needs the session and the
// CSRF token in a header (a JSON body has no form field), and answers 401 and 403
// as JSON instead of redirecting.
func (s *Server) authedJSON(h func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.opt.Auth.Session(r)
		if !ok {
			jsonError(w, http.StatusUnauthorized, "sign in first")
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(sess.CSRF)) != 1 {
			jsonError(w, http.StatusForbidden, "this request did not come from the page: reload it and try again")
			return
		}
		h(w, r, sess)
	}
}

// checkWrite refuses a POST that another site sent, or that lacks the token.
func (s *Server) checkWrite(r *http.Request, sess Session) error {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return errForbidden
	}
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		u, err := url.Parse(o)
		if err != nil || !sameHost(u.Host, r.Host) {
			return errForbidden
		}
	} else if o == "null" {
		return errForbidden
	}
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		return errBadForm
	}
	if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(sess.CSRF)) != 1 {
		return errForbidden
	}
	return nil
}

func sameHost(a, b string) bool { return strings.EqualFold(a, b) }

var (
	errForbidden = &httpError{status: http.StatusForbidden, msg: "this request did not come from the page: reload it and try again"}
	errBadForm   = &httpError{status: http.StatusBadRequest, msg: "the form is incomplete: reload the page and try again"}
)

// httpError is a refusal the UI makes itself, as opposed to the service's.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

// fail renders an error page. The service's errors say what is wrong in words a
// person can act on (a conflict, a missing task); anything else is an internal
// error, reported to OnError and shown generically.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, sess Session, err error) {
	status, msg := http.StatusInternalServerError, "something went wrong; the supervisor's log has the details"
	var he *httpError
	var coded interface{ ExitCode() int }
	switch {
	case errors.As(err, &he):
		status, msg = he.status, he.msg
	case errors.As(err, &coded):
		status, msg = statusOf(coded.ExitCode()), err.Error()
	default:
		if s.opt.OnError != nil {
			s.opt.OnError(err)
		}
	}
	s.render(w, r, status, errorPage(sess.CSRF, status, msg))
}

// statusOf maps an exit code of internal/exitcode to an HTTP status, as the JSON
// API does.
func statusOf(code int) int {
	switch code {
	case 2:
		return http.StatusBadRequest
	case 3:
		return http.StatusNotFound
	case 4:
		return http.StatusUnauthorized
	case 5, 6:
		return http.StatusConflict
	case 7:
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}

// render writes a page. It renders into a buffer first, so a template that fails
// halfway never sends half a page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	var buf bytes.Buffer
	if err := c.Render(r.Context(), &buf); err != nil {
		if s.opt.OnError != nil {
			s.opt.OnError(fmt.Errorf("render %s: %w", r.URL.Path, err))
		}
		http.Error(w, "the page could not be shown", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// seeOther redirects after a write, so a reload never repeats it.
func seeOther(w http.ResponseWriter, r *http.Request, location string) {
	http.Redirect(w, r, location, http.StatusSeeOther) //nolint:gosec // location is a path this package builds from IDs, never from the request
}
