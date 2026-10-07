package kube

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/zerosuxx/kainban/internal/auth"
)

const ns = "kainban"

var fixedNow = time.Date(2026, 10, 8, 12, 30, 0, 0, time.FixedZone("CEST", 2*3600))

func newTestStore(objs ...runtime.Object) (*Store, *fake.Clientset) {
	cs := fake.NewClientset(objs...)
	s := NewStore(cs, ns)
	s.now = func() time.Time { return fixedNow }
	return s, cs
}

func TestGetNotFound(t *testing.T) {
	s, _ := newTestStore()
	_, err := s.Get(context.Background(), "nope")
	if !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("err = %v, want wrapping auth.ErrNotFound", err)
	}
}

func TestGetReturnsDataAndAnnotations(t *testing.T) {
	s, _ := newTestStore(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: ns, Annotations: map[string]string{"a": "b"}},
		Data:       map[string][]byte{"k": []byte("v")},
	})
	got, err := s.Get(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data["k"]) != "v" || got.Annotations["a"] != "b" {
		t.Fatalf("got %+v", got)
	}
}

func TestGetOtherError(t *testing.T) {
	s, cs := newTestStore()
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	_, err := s.Get(context.Background(), "x")
	if err == nil || errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestPutCreates(t *testing.T) {
	s, cs := newTestStore()
	in := &auth.Secret{
		Data:        map[string][]byte{"token": []byte("t")},
		Annotations: map[string]string{auth.AnnotationExpiresAt: "2027-01-01T00:00:00Z"},
	}
	if err := s.Put(context.Background(), "codex", "codex-auth", in); err != nil {
		t.Fatal(err)
	}
	if _, ok := in.Annotations[auth.AnnotationUpdatedAt]; ok {
		t.Fatal("caller's annotations were mutated")
	}
	sec, err := cs.CoreV1().Secrets(ns).Get(context.Background(), "codex-auth", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sec.Type != corev1.SecretTypeOpaque {
		t.Errorf("type = %q", sec.Type)
	}
	if sec.Labels[auth.LabelPartOf] != auth.PartOfValue || sec.Labels[auth.LabelProvider] != "codex" {
		t.Errorf("labels = %v", sec.Labels)
	}
	if got := sec.Annotations[auth.AnnotationUpdatedAt]; got != "2026-10-08T10:30:00Z" {
		t.Errorf("updated-at = %q", got)
	}
	if sec.Annotations[auth.AnnotationExpiresAt] != "2027-01-01T00:00:00Z" {
		t.Errorf("annotations = %v", sec.Annotations)
	}
	if string(sec.Data["token"]) != "t" {
		t.Errorf("data = %v", sec.Data)
	}
}

func TestPutUpdateReplacesDataKeepsLabels(t *testing.T) {
	s, cs := newTestStore(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gh", Namespace: ns,
			Labels:      map[string]string{"team": "x", auth.LabelProvider: "old"},
			Annotations: map[string]string{auth.AnnotationExpiresAt: "old"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"old": []byte("1"), "token": []byte("a")},
	})
	err := s.Put(context.Background(), "github", "gh", &auth.Secret{Data: map[string][]byte{"token": []byte("b")}})
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := cs.CoreV1().Secrets(ns).Get(context.Background(), "gh", metav1.GetOptions{})
	if len(sec.Data) != 1 || string(sec.Data["token"]) != "b" {
		t.Errorf("data = %v", sec.Data)
	}
	if sec.Labels["team"] != "x" || sec.Labels[auth.LabelProvider] != "github" || sec.Labels[auth.LabelPartOf] != auth.PartOfValue {
		t.Errorf("labels = %v", sec.Labels)
	}
	if _, ok := sec.Annotations[auth.AnnotationExpiresAt]; ok {
		t.Errorf("stale annotation kept: %v", sec.Annotations)
	}
	if sec.Annotations[auth.AnnotationUpdatedAt] != "2026-10-08T10:30:00Z" {
		t.Errorf("annotations = %v", sec.Annotations)
	}
}

func TestPutRetriesConflictOnce(t *testing.T) {
	s, cs := newTestStore(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: ns}})
	calls := 0
	cs.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "c", errors.New("stale"))
		}
		return false, nil, nil
	})
	if err := s.Put(context.Background(), "p", "c", &auth.Secret{Data: map[string][]byte{"k": []byte("v")}}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("update calls = %d, want 2", calls)
	}
	sec, _ := cs.CoreV1().Secrets(ns).Get(context.Background(), "c", metav1.GetOptions{})
	if string(sec.Data["k"]) != "v" {
		t.Errorf("data = %v", sec.Data)
	}
}
