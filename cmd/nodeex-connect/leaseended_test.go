package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLeaseEnded(t *testing.T) {
	body := `{"leases":[{"id":"a","status":"active"},{"id":"b","status":"ended"}]}`
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()
	s := session{API: srv.URL, Token: "t"}
	ctx := context.Background()
	if ended, known := leaseEnded(ctx, s, "a"); ended || !known {
		t.Errorf("active lease: ended=%v known=%v", ended, known)
	}
	if ended, known := leaseEnded(ctx, s, "b"); !ended || !known {
		t.Errorf("ended lease: ended=%v known=%v", ended, known)
	}
	// A lease the platform does not list, or a platform that cannot be
	// asked, is no reason to disconnect.
	if ended, known := leaseEnded(ctx, s, "c"); ended || known {
		t.Errorf("unlisted lease: ended=%v known=%v", ended, known)
	}
	fail = true
	if ended, known := leaseEnded(ctx, s, "b"); ended || known {
		t.Errorf("platform down: ended=%v known=%v", ended, known)
	}
}
