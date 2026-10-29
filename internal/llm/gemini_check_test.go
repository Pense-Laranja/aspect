package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckModelReadsTheModelWithTheKeyInAHeader(t *testing.T) {
	var gotPath, gotKey, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey, gotQuery = r.URL.Path, r.Header.Get("x-goog-api-key"), r.URL.RawQuery
		if r.Method != http.MethodGet {
			t.Errorf("method %s, want GET: a check sends no prompt", r.Method)
		}
		w.Write([]byte(`{"name": "models/gemini-flash-latest", "inputTokenLimit": 1048576}`))
	}))
	defer srv.Close()
	g := NewGemini("k-123", WithGeminiBaseURL(srv.URL), WithGeminiModel("gemini-flash-latest"))
	info, err := g.CheckModel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.InputTokenLimit != 1048576 || info.Name != "models/gemini-flash-latest" {
		t.Errorf("info = %+v", info)
	}
	if gotPath != "/models/gemini-flash-latest" || gotKey != "k-123" || gotQuery != "" {
		t.Errorf("path %q key %q query %q: the key goes in the header, never the URL", gotPath, gotKey, gotQuery)
	}
}

func TestCheckModelReportsTheRefusalWithoutTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error": {"message": "API key not valid"}}`))
	}))
	defer srv.Close()
	_, err := NewGemini("secret-key", WithGeminiBaseURL(srv.URL)).CheckModel(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "API key not valid") {
		t.Fatalf("err = %v, want the status and the API's message", err)
	}
	if strings.Contains(err.Error(), "secret-key") {
		t.Error("the error quotes the key")
	}
	if _, err := NewGemini("").CheckModel(context.Background()); !errors.Is(err, ErrNoGeminiKey) {
		t.Errorf("no key: err = %v, want ErrNoGeminiKey", err)
	}
}
