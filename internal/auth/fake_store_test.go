package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// fakeStore is an in-memory Store.
type fakeStore struct {
	secrets map[string]*Secret
	err     error // returned by Get for every name when set
}

func newFakeStore() *fakeStore { return &fakeStore{secrets: map[string]*Secret{}} }

func (s *fakeStore) Get(_ context.Context, name string) (*Secret, error) {
	if s.err != nil {
		return nil, s.err
	}
	sec, ok := s.secrets[name]
	if !ok {
		return nil, fmt.Errorf("get %s: %w", name, ErrNotFound)
	}
	return sec, nil
}

func (s *fakeStore) Put(_ context.Context, _, name string, sec *Secret) error {
	s.secrets[name] = sec
	return nil
}

func (s *fakeStore) with(name, key, val string, ann map[string]string) *fakeStore {
	s.secrets[name] = &Secret{Data: map[string][]byte{key: []byte(val)}, Annotations: ann}
	return s
}

// fixedNow pins nowFunc for the duration of a test.
func fixedNow(t *testing.T, now time.Time) {
	t.Helper()
	old := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = old })
}

var testNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(b) + ".c2ln"
}
