package kube

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretExists mówi, czy Secret istnieje.
func (c *Cluster) SecretExists(ctx context.Context, namespace, name string) (bool, error) {
	_, err := c.core.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// SecretData zwraca dane Secretu.
func (c *Cluster) SecretData(ctx context.Context, namespace, name string) (map[string][]byte, error) {
	s, err := c.core.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return s.Data, nil
}

// StoreSecret zapisuje Secret (tworzy albo podmienia), zakładając
// przestrzeń nazw, jeśli jej nie ma.
func (c *Cluster) StoreSecret(ctx context.Context, namespace, name string, data map[string][]byte) error {
	if err := c.EnsureNamespace(ctx, namespace, nil); err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
	secrets := c.core.CoreV1().Secrets(namespace)
	if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
		return err
	} else if err != nil {
		return err
	}
	return nil
}
