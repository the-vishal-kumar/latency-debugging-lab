package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLimitIsPerClient(t *testing.T) {
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h := limit(ok, 5) // burst of 5
	do := func(client string) int {
		req := httptest.NewRequest(http.MethodGet, "/cpu", nil)
		req.Header.Set("X-Client", client)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := range 5 {
		if c := do("noisy"); c != http.StatusOK {
			t.Fatalf("request %d within the burst: %d", i+1, c)
		}
	}
	if c := do("noisy"); c != http.StatusTooManyRequests {
		t.Fatalf("request past the burst: %d, want 429", c)
	}
	if c := do("quiet"); c != http.StatusOK {
		t.Fatalf("another client must not share the noisy client's bucket: %d", c)
	}
}

func TestStatsRejectsBadTenant(t *testing.T) {
	s := &server{cache: "none", local: map[string]entry{}}
	for _, q := range []string{"", "?tenant=abc", "?tenant=0", "?tenant=-1"} {
		rec := httptest.NewRecorder()
		s.mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stats"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("/stats%s: %d, want 400", q, rec.Code)
		}
	}
}

func TestCPU(t *testing.T) {
	s := &server{iter: 10}
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/cpu", nil))
	if rec.Code != http.StatusOK || len(rec.Body.String()) != len(`{"digest":"01234567"}`) {
		t.Errorf("/cpu: %d %q", rec.Code, rec.Body.String())
	}
}
