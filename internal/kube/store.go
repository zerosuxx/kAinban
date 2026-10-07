// Package kube implements auth.Store on Kubernetes Secrets and builds the
// client kAinban talks to the cluster with (client.go).
package kube

import (
	"context"
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/zerosuxx/kainban/internal/auth"
)

// Store implements auth.Store on Secrets in a single namespace.
type Store struct {
	client    kubernetes.Interface
	namespace string
	now       func() time.Time
}

var _ auth.Store = (*Store)(nil)

// NewStore returns a Store writing Secrets in namespace.
func NewStore(client kubernetes.Interface, namespace string) *Store {
	return &Store{client: client, namespace: namespace, now: time.Now}
}

// Namespace returns the namespace the store works in.
func (s *Store) Namespace() string { return s.namespace }

// Get returns the Secret's data and annotations, or an error wrapping
// auth.ErrNotFound when it does not exist.
func (s *Store) Get(ctx context.Context, name string) (*auth.Secret, error) {
	sec, err := s.client.CoreV1().Secrets(s.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("secret %s/%s: %w", s.namespace, name, auth.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get secret %s/%s: %w", s.namespace, name, err)
	}
	return &auth.Secret{
		Data:        maps.Clone(sec.Data),
		Annotations: maps.Clone(sec.Annotations),
	}, nil
}

// Put creates the Secret or replaces its data, labels and annotations.
// Other labels already on the Secret are kept. A conflicting concurrent
// update is retried once.
func (s *Store) Put(ctx context.Context, provider, name string, sec *auth.Secret) error {
	if sec == nil {
		sec = &auth.Secret{}
	}
	err := s.put(ctx, provider, name, sec)
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		err = s.put(ctx, provider, name, sec)
	}
	if err != nil {
		return fmt.Errorf("put secret %s/%s: %w", s.namespace, name, err)
	}
	return nil
}

func (s *Store) put(ctx context.Context, provider, name string, sec *auth.Secret) error {
	secrets := s.client.CoreV1().Secrets(s.namespace)

	annotations := maps.Clone(sec.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[auth.AnnotationUpdatedAt] = s.now().UTC().Format(time.RFC3339)
	data := maps.Clone(sec.Data)
	if data == nil {
		data = map[string][]byte{}
	}

	existing, err := secrets.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		obj := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   s.namespace,
				Labels:      map[string]string{auth.LabelPartOf: auth.PartOfValue, auth.LabelProvider: provider},
				Annotations: annotations,
			},
			Type: corev1.SecretTypeOpaque,
			Data: data,
		}
		_, err = secrets.Create(ctx, obj, metav1.CreateOptions{})
		return err
	case err != nil:
		return err
	}

	obj := existing.DeepCopy()
	if obj.Labels == nil {
		obj.Labels = map[string]string{}
	}
	obj.Labels[auth.LabelPartOf] = auth.PartOfValue
	obj.Labels[auth.LabelProvider] = provider
	obj.Annotations = annotations
	obj.Data = data
	obj.StringData = nil
	if obj.Type == "" {
		obj.Type = corev1.SecretTypeOpaque
	}
	_, err = secrets.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}
