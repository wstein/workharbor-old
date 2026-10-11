package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/wstein/workharbor/internal/domain"
	"github.com/wstein/workharbor/internal/initiation"
	"github.com/wstein/workharbor/internal/passkey"
	"github.com/wstein/workharbor/internal/service"
	"github.com/wstein/workharbor/internal/store"
)

// Passkeys is what the UI needs of the passkey ceremonies (design D45); the real one
// is *passkey.Service. Enrolment is only started from the host's CLI (a one-time
// token); the web offers no way to add or remove a passkey.
type Passkeys interface {
	Enrolled(ctx context.Context) (bool, error)
	CheckToken(token string) bool
	EnrolBegin(ctx context.Context, token string) (any, string, error)
	EnrolFinish(ctx context.Context, ceremony string, r *http.Request) (passkey.Info, error)
	LoginBegin(ctx context.Context) (any, string, error)
	LoginFinish(ctx context.Context, ceremony string, r *http.Request) (string, error)
	StepUpBegin(ctx context.Context, holder string, b passkey.Binding) (any, string, error)
	StepUpFinish(ctx context.Context, ceremony, holder string, r *http.Request) (passkey.Binding, error)
}

// passkeyMode reports whether web sign-in is by passkey: once one is enrolled the
// token no longer signs in to the web UI (no password fallback, D45); the token
// stays for the host's CLI.
func (s *Server) passkeyMode(ctx context.Context) bool {
	if s.opt.RequirePasskey {
		return true
	}
	if s.opt.Passkeys == nil {
		return false
	}
	ok, err := s.opt.Passkeys.Enrolled(ctx)
	// An error fails closed: if the store cannot say whether a passkey is enrolled,
	// the token does not sign in, because it might be the very thing a passkey replaced.
	return err != nil || ok
}

// stepUpAvailable says whether a review can be answered here: a passkey is enrolled.
func (s *Server) stepUpAvailable(ctx context.Context) bool { return s.passkeyMode(ctx) }

// sensitive reports whether answering a Decision needs a fresh passkey assertion
// (D45): "Ready to push?", an egress host and a feature source. Policy and preset
// changes are not Decisions: they are confirmed on /changes, each with its own
// passkey assertion. A web session alone never answers or confirms them.
func sensitive(d *domain.Decision) bool {
	return d.Kind == domain.DecisionReview || d.Cause == domain.CauseEgressRequest || d.Cause == domain.CauseFeatureSource
}

// bindsTo is what the challenge of a sensitive Decision names besides its ID: the
// commit of a review, the host of an egress request, the reference and digest of a feature source, so an approval for one cannot
// be used for another.
func bindsTo(d *domain.Decision) string {
	if d.Cause == domain.CauseEgressRequest {
		return "host:" + d.Host
	}
	if d.Cause == domain.CauseFeatureSource {
		return "feature:" + d.Feature + "@" + d.FeatureDigest
	}
	return d.SHA
}

// stepUpFor says in words what a step-up is for, for the page.
func stepUpFor(d domain.Decision) string {
	switch {
	case d.Cause == domain.CauseEgressRequest:
		return "this host"
	case d.Cause == domain.CauseFeatureSource:
		return "this feature source"
	case d.Kind == domain.DecisionReview:
		return "this commit"
	}
	return ""
}

const jsonLimit = 1 << 16

// jsonReply writes a JSON answer to the page's script. Nothing in it is secret.
func jsonReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	jsonReply(w, status, map[string]string{"error": msg})
}

// sameOrigin refuses a request another site sent. The ceremonies check the origin
// again in the signed client data; this stops the request itself.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		return err == nil && sameHost(u.Host, r.Host)
	}
	return true
}

func (s *Server) enrolPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	ok := s.opt.Passkeys != nil && token != "" && s.opt.Passkeys.CheckToken(token)
	if !ok {
		token = ""
	}
	s.render(w, r, http.StatusOK, enrolView(token))
}

// enrolBegin spends the one-time token and returns the registration options. It
// needs no session: the token, minted on the host, is the proof.
func (s *Server) enrolBegin(w http.ResponseWriter, r *http.Request) {
	if s.opt.Passkeys == nil || !sameOrigin(r) {
		jsonError(w, http.StatusForbidden, "not available")
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, jsonLimit)).Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "the request is not valid")
		return
	}
	opts, cer, err := s.opt.Passkeys.EnrolBegin(r.Context(), in.Token)
	if err != nil {
		jsonError(w, passkeyStatus(err, http.StatusForbidden), passkeyMessage(err))
		return
	}
	jsonReply(w, http.StatusOK, map[string]any{"options": opts, "ceremony": cer})
}

func (s *Server) enrolFinish(w http.ResponseWriter, r *http.Request) {
	if s.opt.Passkeys == nil || !sameOrigin(r) {
		jsonError(w, http.StatusForbidden, "not available")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, jsonLimit)
	info, err := s.opt.Passkeys.EnrolFinish(r.Context(), r.Header.Get("X-Ceremony"), r)
	if err != nil {
		jsonError(w, passkeyStatus(err, http.StatusForbidden), passkeyMessage(err))
		return
	}
	jsonReply(w, http.StatusOK, map[string]string{"name": info.Name})
}

func (s *Server) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if s.opt.Passkeys == nil || !sameOrigin(r) {
		jsonError(w, http.StatusForbidden, "not available")
		return
	}
	opts, cer, err := s.opt.Passkeys.LoginBegin(r.Context())
	if err != nil {
		jsonError(w, passkeyStatus(err, http.StatusForbidden), passkeyMessage(err))
		return
	}
	jsonReply(w, http.StatusOK, map[string]any{"options": opts, "ceremony": cer})
}

// passkeyLoginFinish signs in with a verified assertion: a session cookie, and
// nothing else. Sign-in by passkey is refused when none is enrolled and the token
// form is still the way in.
func (s *Server) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	starter, ok := s.opt.Auth.(Starter)
	if s.opt.Passkeys == nil || !ok || !sameOrigin(r) {
		jsonError(w, http.StatusForbidden, "not available")
		return
	}
	r = s.stamp(r) // before the assertion is checked: a revocation after it must stop this sign-in
	r.Body = http.MaxBytesReader(w, r.Body, jsonLimit)
	keyID, err := s.opt.Passkeys.LoginFinish(r.Context(), r.Header.Get("X-Ceremony"), r)
	if err != nil {
		jsonError(w, passkeyStatus(err, http.StatusUnauthorized), passkeyMessage(err))
		return
	}
	if !starter.StartFor(w, r, keyID) {
		jsonError(w, http.StatusConflict, ErrSignInChanged.Error())
		return
	}
	jsonReply(w, http.StatusOK, map[string]string{"next": "/"})
}

// passkeyMessage is what the page's script may show: a short sentence for the
// refusals the human can act on, and nothing from the library's internals.
// passkeyStatus is the HTTP status of a refused ceremony: 429 when the limits
// stopped it, else the handler's own.
func passkeyStatus(err error, otherwise int) int {
	if errors.Is(err, passkey.ErrTooMany) || errors.Is(err, passkey.ErrBusy) {
		return http.StatusTooManyRequests
	}
	return otherwise
}

func passkeyMessage(err error) string {
	switch {
	case errors.Is(err, passkey.ErrBadToken):
		return "This enrolment link is not valid, has expired or was already used. Run `whr passkey add` on the host again."
	case errors.Is(err, passkey.ErrBadCeremony):
		return "That took too long or was already used. Try again."
	case errors.Is(err, passkey.ErrTooMany), errors.Is(err, passkey.ErrBusy):
		return "Too many sign-in attempts. Wait a minute and try again."
	case errors.Is(err, passkey.ErrNotEnrolled):
		return "No passkey is enrolled. Enrol one from the host with `whr passkey add`."
	case errors.Is(err, passkey.ErrCloned):
		return "That passkey's counter went backwards, so it was refused: it may have been copied."
	case errors.Is(err, passkey.ErrNotVerified):
		return "The passkey did not verify you (Face ID, a fingerprint or a PIN is required)."
	}
	return "The passkey was refused."
}

// stepUpBegin starts a fresh passkey assertion for one open Decision that needs it.
// The challenge names the Decision and its commit SHA and belongs to this session.
func (s *Server) stepUpBegin(w http.ResponseWriter, r *http.Request, sess Session) {
	if s.opt.Passkeys == nil || !sameOrigin(r) {
		jsonError(w, http.StatusForbidden, "not available")
		return
	}
	d, err := s.openDecision(r)
	if err != nil {
		jsonError(w, http.StatusNotFound, "that decision is not open")
		return
	}
	if !sensitive(d) {
		jsonError(w, http.StatusBadRequest, "that decision needs no passkey")
		return
	}
	s.beginStepUp(w, r, sess, passkey.Binding{Decision: string(d.ID), SHA: bindsTo(d)})
}

func (s *Server) openDecision(r *http.Request) (*domain.Decision, error) {
	id := domain.ID(r.PathValue("decision"))
	open, err := s.be.Inbox(r.Context())
	if err != nil {
		return nil, err
	}
	for i := range open {
		if open[i].ID == id {
			return &open[i], nil
		}
	}
	return nil, &domain.NotFoundError{Kind: "open decision", ID: string(id)}
}

// stepUpFinish verifies the assertion and only then records the answer. What the
// challenge named must be the Decision being answered and the SHA it has now: an
// assertion for another Decision, or for an earlier commit, is refused. The
// answer carries the SHA, so the service checks it again (a review allows exactly
// the commit it was shown).
func (s *Server) stepUpFinish(w http.ResponseWriter, r *http.Request, sess Session) {
	if s.opt.Passkeys == nil || !sameOrigin(r) {
		jsonError(w, http.StatusForbidden, "not available")
		return
	}
	q := r.URL.Query()
	option, reason := q.Get("option"), strings.TrimSpace(q.Get("reason"))
	d, err := s.openDecision(r)
	if err != nil {
		jsonError(w, http.StatusNotFound, "that decision is not open")
		return
	}
	if !sensitive(d) || !contains(d.Options, option) {
		jsonError(w, http.StatusBadRequest, "that is not an answer to this decision")
		return
	}
	s.finishStepUp(w, r, sess, passkey.Binding{Decision: string(d.ID), SHA: bindsTo(d)}, option+"|"+reason, func() (string, error) {
		_, err := s.be.Answer(initiation.With(r.Context(), initiation.UserAction(actorOf(sess), "web")), d.ID, domain.Response{By: "web+passkey", Option: option, Reason: reason, SHA: d.SHA, At: s.opt.Now()})
		if err != nil {
			return "", err
		}
		return "/inbox?flash=answered", nil
	})
}

// finishStepUp verifies the assertion and only then runs the action, once. What the
// challenge named must be exactly what the page is acting on now (want): an
// assertion for another Decision, change or commit is refused. The ceremony is
// single-use and its key makes a repeat of the request answer once.
func (s *Server) finishStepUp(w http.ResponseWriter, r *http.Request, sess Session, want passkey.Binding, detail string, run func() (location string, err error)) {
	cer := r.Header.Get("X-Ceremony")
	r.Body = http.MaxBytesReader(w, r.Body, jsonLimit)
	got, err := s.opt.Passkeys.StepUpFinish(r.Context(), cer, sess.CSRF, r)
	if err != nil {
		jsonError(w, passkeyStatus(err, http.StatusForbidden), passkeyMessage(err))
		return
	}
	if got != want {
		jsonError(w, http.StatusForbidden, "The approval was for another decision, change, commit or host.")
		return
	}
	loc, err := s.onceKey(r.Context(), "stepup:"+cer, want.Decision+"|"+want.SHA+"|"+detail, run)
	if err != nil {
		var coded interface{ ExitCode() int }
		switch {
		case errors.As(err, &coded):
			jsonError(w, statusOf(coded.ExitCode()), err.Error())
		case errors.Is(err, store.ErrNoChange), errors.Is(err, store.ErrStaleChange):
			jsonError(w, http.StatusConflict, "That change is no longer waiting, or the repository changed since it was raised. Reload the page.")
		case errors.Is(err, service.ErrNoRevoker):
			jsonError(w, http.StatusConflict, "There is no forge token to revoke.")
		default:
			if s.opt.OnError != nil {
				s.opt.OnError(err)
			}
			jsonError(w, http.StatusInternalServerError, "The answer could not be recorded.")
		}
		return
	}
	jsonReply(w, http.StatusOK, map[string]string{"location": loc})
}
