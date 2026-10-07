package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func geminiServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models" || r.URL.RawQuery != "" {
			http.Error(w, "unexpected request", http.StatusTeapot)
			return
		}
		switch r.Header.Get("x-goog-api-key") {
		case "AIzaGOOD":
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "AIzaDOWN":
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		default:
			http.Error(w, `{"error":{"status":"INVALID_ARGUMENT","message":"API key not valid"}}`, http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestValidateGeminiKey(t *testing.T) {
	srv := geminiServer(t)
	tests := []struct {
		key         string
		wantStatus  int
		wantInvalid bool
	}{
		{"AIzaGOOD", 0, false},
		{"AIzaBAD", 400, true},
		{"AIzaDOWN", 503, false},
	}
	for _, tt := range tests {
		err := validateGeminiKey(context.Background(), srv.Client(), srv.URL, tt.key)
		if tt.wantStatus == 0 {
			if err != nil {
				t.Errorf("%s: %v", tt.key, err)
			}
			continue
		}
		var ge *geminiKeyError
		if !errors.As(err, &ge) || ge.Status != tt.wantStatus || ge.Invalid != tt.wantInvalid {
			t.Errorf("%s: err = %#v", tt.key, err)
		}
	}
}

func TestAntigravityCheck(t *testing.T) {
	srv := geminiServer(t)
	p := newAntigravity(srv.Client(), srv.URL)
	tests := []struct {
		name   string
		store  *fakeStore
		live   bool
		want   State
		detail string
	}{
		{"missing", newFakeStore(), true, StateMissing, ""},
		{"empty", newFakeStore().with(AntigravitySecretName, AntigravityKeyKey, "", nil), false, StateInvalid, ""},
		{"local", newFakeStore().with(AntigravitySecretName, AntigravityKeyKey, "AIzaBAD", nil), false, StateValid, "present"},
		{"live ok", newFakeStore().with(AntigravitySecretName, AntigravityKeyKey, "AIzaGOOD", nil), true, StateValid, ""},
		{"live rejected", newFakeStore().with(AntigravitySecretName, AntigravityKeyKey, "AIzaBAD", nil), true, StateInvalid, "HTTP 400"},
		{"live outage", newFakeStore().with(AntigravitySecretName, AntigravityKeyKey, "AIzaDOWN", nil), true, StateUnknown, "HTTP 503"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := p.Check(context.Background(), tt.store, tt.live)
			if st.State != tt.want || !strings.Contains(st.Detail, tt.detail) {
				t.Fatalf("got %v %q", st.State, st.Detail)
			}
		})
	}
	srv.Close()
	st := p.Check(context.Background(), newFakeStore().with(AntigravitySecretName, AntigravityKeyKey, "AIzaGOOD", nil), true)
	if st.State != StateUnknown || strings.Contains(st.Detail, "AIzaGOOD") {
		t.Fatalf("network error: %v %q", st.State, st.Detail)
	}
}

func TestAntigravityFlow(t *testing.T) {
	srv := geminiServer(t)
	f := newAntigravity(srv.Client(), srv.URL).NewFlow()
	ctx := context.Background()
	if s, err := f.Next(ctx, ""); err != nil || s.Kind != StepInput || !s.Secret {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := f.Next(ctx, "  "); err == nil {
		t.Fatal("want error for empty key")
	}
	if _, err := f.Next(ctx, "AIzaBAD"); err == nil {
		t.Fatal("want error for rejected key")
	}
	s, err := f.Next(ctx, " AIzaGOOD\n")
	if err != nil || s.Kind != StepDone || string(s.Secrets[AntigravitySecretName].Data[AntigravityKeyKey]) != "AIzaGOOD" {
		t.Fatalf("%+v %v", s, err)
	}
}
