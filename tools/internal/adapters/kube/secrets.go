package kube

import (
	"context"

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
