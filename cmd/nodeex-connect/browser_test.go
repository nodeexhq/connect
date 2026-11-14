package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBrowserLogin(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/device":
			json.NewEncoder(w).Encode(map[string]any{"device_code": "dc", "user_code": "BCDF-GHJK",
				"verification_url": "https://nodeex.example/authorize?code=BCDF-GHJK", "expires_in": 30, "interval": 1})
		case "/v1/auth/device/token":
			var in struct {
				DeviceCode string `json:"device_code"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			if in.DeviceCode != "dc" {
				t.Errorf("device code %q", in.DeviceCode)
			}
			if polls.Add(1) < 2 {
				w.WriteHeader(http.StatusPreconditionRequired)
				w.Write([]byte(`{"error":{"code":"authorization_pending","message":"waiting"}}`))
				return
			}
			w.Write([]byte(`{"token":"tok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tok, err := browserLogin(ctx, srv.URL, true)
	if err != nil || tok != "tok" {
		t.Fatalf("token %q, err %v", tok, err)
	}
}

func TestOpenBrowserRefusesNonWeb(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "javascript:alert(1)", "calc.exe", "https://"} {
		if openBrowser(u) == nil {
			t.Errorf("opened %q", u)
		}
	}
}
