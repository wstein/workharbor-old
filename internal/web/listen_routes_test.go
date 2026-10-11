package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConfiguredWebAddressesKeepSignInAndExcludeJSONAPI(t *testing.T) {
	r := newRig(t)
	for _, host := range []string{"127.0.0.1:8787", "10.44.0.1:8787", "[fd42::1]:8787"} {
		for _, bearer := range []string{"", "Bearer " + token} {
			for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
				for _, path := range []string{"/v1", "/v1/", "/v1/health", "/v1/tasks", "/v1/passkeys/enrolments", "/v1/decisions/d1/answer"} {
					req := httptest.NewRequestWithContext(bg, method, "http://"+host+path, nil)
					req.Header.Set("Authorization", bearer)
					rec := httptest.NewRecorder()
					r.srv.Config.Handler.ServeHTTP(rec, req)
					if rec.Code != http.StatusNotFound {
						t.Errorf("%s %s on %s (token %v) = %d", method, path, host, bearer != "", rec.Code)
					}
				}
			}
			req := httptest.NewRequestWithContext(bg, http.MethodGet, "http://"+host+"/", nil)
			req.Header.Set("Authorization", bearer)
			rec := httptest.NewRecorder()
			r.srv.Config.Handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
				t.Errorf("web without session on %s = %d %s", host, rec.Code, rec.Header().Get("Location"))
			}
		}
	}
}
