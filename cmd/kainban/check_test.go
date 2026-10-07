package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zerosuxx/kainban/internal/auth"
)

type fakeStore map[string]*auth.Secret

func (f fakeStore) Get(_ context.Context, name string) (*auth.Secret, error) {
	if s, ok := f[name]; ok {
		return s, nil
	}
	return nil, auth.ErrNotFound
}

func (f fakeStore) Put(_ context.Context, _, name string, s *auth.Secret) error {
	f[name] = s
	return nil
}

// fakeProvider reports missing when its Secret is absent, else the state
// stored in the Secret's "state" key.
type fakeProvider struct {
	id      string
	expires *time.Time
}

func (p fakeProvider) ID() string            { return p.id }
func (p fakeProvider) Title() string         { return strings.ToUpper(p.id) }
func (p fakeProvider) SecretNames() []string { return []string{p.id + "-auth"} }
func (p fakeProvider) NewFlow() auth.Flow    { return nil }
func (p fakeProvider) Check(ctx context.Context, store auth.Store, live bool) auth.Status {
	s, err := store.Get(ctx, p.id+"-auth")
	if errors.Is(err, auth.ErrNotFound) {
		return auth.Status{State: auth.StateMissing, Detail: "not configured"}
	}
	st := map[string]auth.State{"valid": auth.StateValid, "invalid": auth.StateInvalid}[string(s.Data["state"])]
	if string(s.Data["state"]) == "unknown" {
		st = auth.StateUnknown
	}
	d := "local"
	if live {
		d = "live"
	}
	return auth.Status{State: st, Detail: d, ExpiresAt: p.expires}
}

func secret(state string) *auth.Secret {
	return &auth.Secret{Data: map[string][]byte{"state": []byte(state)}}
}

func TestChecks(t *testing.T) {
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	exp := now.Add(9 * 24 * time.Hour)
	providers := []auth.Provider{
		fakeProvider{id: "a", expires: &exp},
		fakeProvider{id: "b"},
	}

	cases := []struct {
		name       string
		store      fakeStore
		requireAll bool
		want       int
	}{
		{"all valid", fakeStore{"a-auth": secret("valid"), "b-auth": secret("valid")}, false, 0},
		{"missing skipped", fakeStore{"a-auth": secret("valid")}, false, 0},
		{"missing required", fakeStore{"a-auth": secret("valid")}, true, 1},
		{"invalid", fakeStore{"a-auth": secret("valid"), "b-auth": secret("invalid")}, false, 1},
		{"unknown", fakeStore{"a-auth": secret("unknown")}, false, 1},
		{"none", fakeStore{}, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := runChecks(context.Background(), providers, c.store, false)
			if got := checkExitCode(res, c.requireAll); got != c.want {
				t.Fatalf("exit = %d, want %d (%+v)", got, c.want, res)
			}
		})
	}

	res := runChecks(context.Background(), providers, fakeStore{"a-auth": secret("valid")}, true)
	if res[0].ID != "a" || res[1].ID != "b" || res[0].Detail != "live" {
		t.Fatalf("results = %+v", res)
	}

	var buf bytes.Buffer
	if err := writeCheckTable(&buf, res, now); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"ID", "STATE", "valid", "missing", "not configured", "2026-10-17T00:00:00Z (in 9d)"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}

	buf.Reset()
	if err := writeCheckJSON(&buf, res); err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 || decoded[0]["state"] != "valid" || decoded[1]["state"] != "missing" {
		t.Fatalf("json = %s", buf.String())
	}
	if _, ok := decoded[1]["expires_at"]; ok {
		t.Errorf("expires_at should be omitted when unknown")
	}
}

func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"bogus"}, {"auth", "nope"}, {"auth", "--bad"}, {"auth", "check", "extra"}} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb); code != 2 {
			t.Errorf("run(%v) = %d, want 2", args, code)
		}
		if !strings.Contains(errb.String(), "Usage:") {
			t.Errorf("run(%v): no usage", args)
		}
	}
	var out bytes.Buffer
	if code := run(context.Background(), []string{"version"}, &out, &out); code != 0 || strings.TrimSpace(out.String()) != "dev" {
		t.Errorf("version: %d %q", code, out.String())
	}
}
