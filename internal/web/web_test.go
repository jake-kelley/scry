package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"scry/internal/query"
)

func TestIndexPageServesHTML(t *testing.T) {
	mux := NewMux(func(q string, limit int) ([]query.Result, error) { return nil, nil })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "<html") {
		t.Error("body does not look like the embedded HTML page")
	}
}

func TestSearchReturnsValidJSON(t *testing.T) {
	want := []query.Result{
		{Name: "report.txt", Path: "/root/report.txt", Score: 42, Size: 123, MTime: 456},
	}
	var gotQuery string
	var gotLimit int
	mux := NewMux(func(q string, limit int) ([]query.Result, error) {
		gotQuery = q
		gotLimit = limit
		return want, nil
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/search?q=report&limit=5", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /search status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if gotQuery != "report" || gotLimit != 5 {
		t.Errorf("search called with (%q, %d), want (%q, 5)", gotQuery, gotLimit, "report")
	}

	var got []resultJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v (body: %s)", err, rec.Body.String())
	}
	if len(got) != 1 || got[0].Name != "report.txt" || got[0].Path != "/root/report.txt" {
		t.Errorf("results = %+v, want one report.txt", got)
	}
}

func TestSearchErrorReturnsValidJSON(t *testing.T) {
	mux := NewMux(func(q string, limit int) ([]query.Result, error) {
		return nil, &parseError{"bad query"}
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/search?q=%22unterminated", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON on error path: %v (body: %s)", err, rec.Body.String())
	}
	if body["error"] == "" {
		t.Error("expected a non-empty error field")
	}
}

type parseError struct{ msg string }

func (e *parseError) Error() string { return e.msg }

func TestValidateLoopback(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:8973", true},
		{"localhost:8973", true},
		{"0.0.0.0:8973", false},
		{"192.168.1.5:8973", false},
		{"not-an-addr", false},
	}
	for _, c := range cases {
		err := ValidateLoopback(c.addr)
		if (err == nil) != c.ok {
			t.Errorf("ValidateLoopback(%q) err = %v, want ok=%v", c.addr, err, c.ok)
		}
	}
}
