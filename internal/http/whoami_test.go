package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestWhoami(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/public/whoami", (&Handler{}).Whoami)

	cases := []struct {
		name        string
		headers     map[string]string
		wantIP      string
		wantCountry string
	}{
		{"cloudflare headers", map[string]string{"CF-Connecting-IP": "108.46.200.179", "CF-IPCountry": "us"}, "108.46.200.179", "US"},
		{"unknown country XX", map[string]string{"CF-Connecting-IP": "1.2.3.4", "CF-IPCountry": "XX"}, "1.2.3.4", ""},
		{"tor T1", map[string]string{"CF-Connecting-IP": "1.2.3.4", "CF-IPCountry": "T1"}, "1.2.3.4", ""},
		{"no country header", map[string]string{"CF-Connecting-IP": "2a0b:ee80::3"}, "2a0b:ee80::3", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/public/whoami", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			var body struct{ IP, Country string }
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.IP != tc.wantIP || body.Country != tc.wantCountry {
				t.Errorf("got %+v, want ip=%s country=%s", body, tc.wantIP, tc.wantCountry)
			}
		})
	}

	t.Run("falls back to peer address without CF header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/public/whoami", nil)
		req.RemoteAddr = "203.0.113.9:4321"
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var body struct{ IP string }
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.IP != "203.0.113.9" {
			t.Errorf("ip = %q, want 203.0.113.9", body.IP)
		}
	})
}
