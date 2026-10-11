package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/wstein/workharbor/internal/passkey"
)

func TestRemoteWebNeverFallsBackToAPIToken(t *testing.T) {
	p := newPKRigWith(t, nil, func(o *Options) { o.RequirePasskey = true })
	check := func(stage string) {
		t.Helper()
		for _, host := range []string{"10.44.0.1:8787", "127.0.0.1:8787", "whr.example.test"} {
			req := httptest.NewRequestWithContext(bg, http.MethodPost, "http://"+host+"/login", strings.NewReader(url.Values{"token": {token}}.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("X-Forwarded-Proto", "https")
			rec := httptest.NewRecorder()
			p.srv.Config.Handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
				t.Errorf("%s %s token login = %d cookies=%v", stage, host, rec.Code, rec.Result().Cookies())
			}
			req = httptest.NewRequestWithContext(bg, http.MethodGet, "http://"+host+"/", nil)
			rec = httptest.NewRecorder()
			p.srv.Config.Handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusSeeOther {
				t.Errorf("%s %s unauthenticated dashboard = %d", stage, host, rec.Code)
			}
		}
	}
	check("before enrolment")
	p.enrol(p.browser())
	check("enrolled")
	signedIn := p.signIn()
	if resp, _ := signedIn.do("GET", "/", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("passkey dashboard = %d", resp.StatusCode)
	}
	keys, err := p.svc.List(bg)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %v %v", keys, err)
	}
	if err := p.svc.Revoke(bg, keys[0].ID); err != nil {
		t.Fatal(err)
	}
	check("last passkey revoked")
	if resp, _ := signedIn.do("GET", "/", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoked passkey dashboard = %d", resp.StatusCode)
	}
}

func TestRemoteSessionsAlwaysUseSecureHostCookies(t *testing.T) {
	auth, err := NewTokenAuth([]byte(token), nil)
	if err != nil {
		t.Fatal(err)
	}
	auth.RequireSecureCookies()
	for _, host := range []string{"10.44.0.1:8787", "127.0.0.1:8787", "whr.example.test"} {
		req := httptest.NewRequestWithContext(bg, http.MethodGet, "http://"+host+"/", nil)
		rec := httptest.NewRecorder()
		if !auth.StartFor(rec, req, "key1") {
			t.Fatal("session did not start")
		}
		cookies := rec.Result().Cookies()
		if len(cookies) != 1 || !cookies[0].Secure || cookies[0].Name != "__Host-"+cookieName {
			t.Errorf("%s cookies = %v", host, cookies)
		}
	}
}

func TestRemoteTokenIsRefusedWhenPasskeyStateCannotBeRead(t *testing.T) {
	p := newPKRigWith(t, func(s *passkey.Service) Passkeys { return failingEnrolled{s} }, func(o *Options) { o.RequirePasskey = true })
	resp, _ := p.browser().do("POST", "/login", url.Values{"token": {token}})
	if resp.StatusCode != http.StatusForbidden || len(resp.Cookies()) != 0 {
		t.Fatalf("unreadable remote passkeys: %d cookies=%v", resp.StatusCode, resp.Cookies())
	}
}
