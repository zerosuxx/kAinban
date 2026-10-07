package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func codexAuthJSON(t *testing.T, exp time.Time, refresh string) []byte {
	t.Helper()
	m := map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token":      "idtok",
			"access_token":  makeJWT(t, map[string]any{"exp": exp.Unix(), "sub": "u"}),
			"refresh_token": refresh,
			"account_id":    "acct-1",
		},
		"last_refresh": "2026-10-01T00:00:00Z",
		"future_field": map[string]any{"x": 1},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStripRefreshToken(t *testing.T) {
	full := codexAuthJSON(t, testNow.Add(240*time.Hour), "rt-secret")
	tests := []struct {
		name    string
		in      []byte
		wantErr bool
	}{
		{"full", full, false},
		{"already stripped", codexAuthJSON(t, testNow, ""), false},
		{"no refresh field", []byte(`{"tokens":{"access_token":"a"}}`), false},
		{"not json", []byte(`nope`), true},
		{"array", []byte(`[]`), true},
		{"no tokens", []byte(`{"auth_mode":"chatgpt"}`), true},
		{"tokens not object", []byte(`{"tokens":"x"}`), true},
		{"tokens null", []byte(`{"tokens":null}`), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := StripRefreshToken(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if strings.Contains(string(out), "rt-secret") {
				t.Fatal("refresh token leaked")
			}
			var got, orig map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal(tt.in, &orig)
			tok := got["tokens"].(map[string]any)
			if rt, ok := tok["refresh_token"]; !ok || rt != "" {
				t.Fatalf("refresh_token = %v, %v; want present and empty", rt, ok)
			}
			// every other field survives
			origTok := orig["tokens"].(map[string]any)
			for k, v := range origTok {
				if k != "refresh_token" && tok[k] != v {
					t.Errorf("tokens.%s changed", k)
				}
			}
			for k := range orig {
				if _, ok := got[k]; !ok {
					t.Errorf("field %s dropped", k)
				}
			}
		})
	}
}

func TestAccessTokenExpiry(t *testing.T) {
	exp := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	tok := func(claims map[string]any) []byte {
		b, _ := json.Marshal(map[string]any{"tokens": map[string]any{"access_token": makeJWT(t, claims)}})
		return b
	}
	tests := []struct {
		name    string
		in      []byte
		want    time.Time
		wantErr bool
	}{
		{"ok", codexAuthJSON(t, exp, "r"), exp, false},
		{"float exp", tok(map[string]any{"exp": float64(exp.Unix()) + 0.5}), exp, false},
		{"no exp", tok(map[string]any{"sub": "x"}), time.Time{}, true},
		{"string exp", tok(map[string]any{"exp": "soon"}), time.Time{}, true},
		{"not jwt", []byte(`{"tokens":{"access_token":"opaque"}}`), time.Time{}, true},
		{"bad base64", []byte(`{"tokens":{"access_token":"a.!!!.c"}}`), time.Time{}, true},
		{"empty access token", []byte(`{"tokens":{"access_token":""}}`), time.Time{}, true},
		{"no tokens", []byte(`{}`), time.Time{}, true},
		{"not json", []byte(`x`), time.Time{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AccessTokenExpiry(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestJWTExpiryPadded(t *testing.T) {
	j := makeJWT(t, map[string]any{"exp": 1700000000})
	parts := strings.Split(j, ".")
	for len(parts[1])%4 != 0 {
		parts[1] += "="
	}
	got, err := jwtExpiry(strings.Join(parts, "."))
	if err != nil || got.Unix() != 1700000000 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestParseCallbackURL(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"ok", "http://localhost:1455/auth/callback?code=abc&state=xyz", "/auth/callback?code=abc&state=xyz", false},
		{"keeps raw query", " http://localhost:1455/auth/callback?code=a%2Fb&scope=openid+profile&state=s%3D \n",
			"/auth/callback?code=a%2Fb&scope=openid+profile&state=s%3D", false},
		{"127.0.0.1", "http://127.0.0.1:1455/auth/callback?state=s&code=c", "/auth/callback?state=s&code=c", false},
		{"empty", "", "", true},
		{"wrong path", "http://localhost:1455/success?code=a&state=b", "", true},
		{"missing state", "http://localhost:1455/auth/callback?code=a", "", true},
		{"missing code", "http://localhost:1455/auth/callback?state=a", "", true},
		{"oauth error", "http://localhost:1455/auth/callback?error=access_denied&state=a", "", true},
		{"garbage", "::not a url", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCallbackURL(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), "abc") {
				t.Fatal("error leaks the code")
			}
		})
	}
}

func TestExtractAuthorizeURL(t *testing.T) {
	const u = "https://auth.openai.com/oauth/authorize?response_type=code&client_id=app_X&redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback&state=abc"
	tests := []struct {
		name, out, want string
	}{
		{"plain", "Starting local login server on http://localhost:1455.\nIf your browser did not open, navigate to this URL to authenticate:\n\n" + u + "\n", u},
		{"ansi", "\x1b[94m" + u + "\x1b[0m\n", u},
		{"quoted", `open "` + u + `"`, u},
		{"none yet", "Starting local login server on http://localhost:1455.\n", ""},
		{"other host", "https://example.com/x", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractAuthorizeURL(tt.out); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCallbackBaseFromAuthorizeURL(t *testing.T) {
	const def = "http://localhost:1455"
	tests := []struct{ in, want string }{
		{"https://auth.openai.com/oauth/authorize?redirect_uri=http%3A%2F%2Flocalhost%3A1456%2Fauth%2Fcallback", "http://localhost:1456"},
		{"https://auth.openai.com/oauth/authorize?redirect_uri=http%3A%2F%2F127.0.0.1%3A1455%2Fauth%2Fcallback", "http://127.0.0.1:1455"},
		{"https://auth.openai.com/oauth/authorize?redirect_uri=https%3A%2F%2Fevil.example%2Fcb", def},
		{"https://auth.openai.com/oauth/authorize", def},
	}
	for _, tt := range tests {
		if got := callbackBaseFromAuthorizeURL(tt.in, def); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestForwardCallback(t *testing.T) {
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/callback":
			gotURI = r.RequestURI
			if r.URL.Query().Get("code") == "bad" {
				http.Error(w, "state mismatch", http.StatusBadRequest)
				return
			}
			http.Redirect(w, r, "/success", http.StatusFound)
		case "/success":
			_, _ = w.Write([]byte("ok"))
		}
	}))
	defer srv.Close()

	pq := "/auth/callback?code=a%2Fb&scope=openid+profile&state=s%3D"
	if err := forwardCallback(context.Background(), srv.Client(), srv.URL, pq); err != nil {
		t.Fatal(err)
	}
	if gotURI != pq {
		t.Fatalf("server saw %q, want %q", gotURI, pq)
	}
	if err := forwardCallback(context.Background(), srv.Client(), srv.URL, "/auth/callback?code=bad&state=s"); err == nil {
		t.Fatal("want error on HTTP 400")
	}
	srv.Close()
	if err := forwardCallback(context.Background(), srv.Client(), srv.URL, pq); err == nil {
		t.Fatal("want error when listener is gone")
	}
}

func TestBuildCodexSecrets(t *testing.T) {
	exp := testNow.Add(240 * time.Hour)
	full := codexAuthJSON(t, exp, "rt-secret")
	sec, gotExp, err := buildCodexSecrets(full)
	if err != nil {
		t.Fatal(err)
	}
	if !gotExp.Equal(exp.Truncate(time.Second)) {
		t.Fatalf("exp = %v", gotExp)
	}
	agent, orch := sec[CodexSecretName], sec[CodexRefreshSecretName]
	if agent == nil || orch == nil {
		t.Fatal("missing secret")
	}
	if strings.Contains(string(agent.Data[CodexAuthKey]), "rt-secret") {
		t.Fatal("agent copy has refresh token")
	}
	if string(orch.Data[CodexAuthKey]) != string(full) {
		t.Fatal("orchestrator copy altered")
	}
	if agent.Annotations[AnnotationExpiresAt] != formatExpiry(exp) {
		t.Fatalf("expires-at = %q", agent.Annotations[AnnotationExpiresAt])
	}
	if _, _, err := buildCodexSecrets(codexAuthJSON(t, exp, "")); err == nil {
		t.Fatal("want error without refresh token")
	}
}

func TestCodexCheck(t *testing.T) {
	fixedNow(t, testNow)
	good := testNow.Add(9*24*time.Hour + time.Hour)
	full := codexAuthJSON(t, good, "rt")
	stripped, _ := StripRefreshToken(full)
	expiredFull := codexAuthJSON(t, testNow.Add(-time.Hour), "rt")
	expiredStripped, _ := StripRefreshToken(expiredFull)

	tests := []struct {
		name   string
		store  *fakeStore
		want   State
		detail string
	}{
		{"missing", newFakeStore(), StateMissing, ""},
		{"store error", &fakeStore{err: errors.New("boom")}, StateUnknown, "boom"},
		{"valid", newFakeStore().
			with(CodexSecretName, CodexAuthKey, string(stripped), nil).
			with(CodexRefreshSecretName, CodexAuthKey, string(full), nil), StateValid, "expires in 9d"},
		{"leak", newFakeStore().
			with(CodexSecretName, CodexAuthKey, string(full), nil).
			with(CodexRefreshSecretName, CodexAuthKey, string(full), nil), StateInvalid, "leak"},
		{"expired", newFakeStore().
			with(CodexSecretName, CodexAuthKey, string(expiredStripped), nil).
			with(CodexRefreshSecretName, CodexAuthKey, string(expiredFull), nil), StateInvalid, "expired 1h ago"},
		{"orchestrator missing", newFakeStore().
			with(CodexSecretName, CodexAuthKey, string(stripped), nil), StateInvalid, "orchestrator copy missing"},
		{"orchestrator stripped", newFakeStore().
			with(CodexSecretName, CodexAuthKey, string(stripped), nil).
			with(CodexRefreshSecretName, CodexAuthKey, string(stripped), nil), StateInvalid, "no refresh_token"},
		{"malformed", newFakeStore().
			with(CodexSecretName, CodexAuthKey, "{", nil), StateInvalid, "JSON"},
		{"empty key", newFakeStore().
			with(CodexSecretName, "other", "x", nil), StateInvalid, "no auth.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, live := range []bool{false, true} {
				st := NewCodex().Check(context.Background(), tt.store, live)
				if st.State != tt.want || !strings.Contains(st.Detail, tt.detail) {
					t.Fatalf("live=%v: got %v %q, want %v containing %q", live, st.State, st.Detail, tt.want, tt.detail)
				}
				if tt.want == StateValid && (st.ExpiresAt == nil || !st.ExpiresAt.Equal(good.Truncate(time.Second))) {
					t.Fatalf("ExpiresAt = %v", st.ExpiresAt)
				}
			}
		})
	}
}
