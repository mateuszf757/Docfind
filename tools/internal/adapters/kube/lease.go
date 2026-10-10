package kube

import (
	"context"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
)

// AcquireLease próbuje wziąć blokadę (Lease) dla holder na ttl. Żywa
// blokada innego biegu to Acquired=false z jej właścicielem i terminem;
// wygasła — przejęta z resourceVersion, więc dwa biegi nie przejmą jej
// naraz (drugi dostaje Conflict).
func (c *Cluster) AcquireLease(ctx context.Context, namespace, name, holder string, ttl time.Duration) (cluster.LeaseAttempt, error) {
	leases := c.core.CoordinationV1().Leases(namespace)
	now := metav1.NewMicroTime(time.Now())
	seconds := int32(ttl.Seconds())
	spec := coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &seconds, AcquireTime: &now, RenewTime: &now}
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: spec}
	_, err := leases.Create(ctx, lease, metav1.CreateOptions{})
	if err == nil {
		return cluster.LeaseAttempt{Acquired: true}, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return cluster.LeaseAttempt{}, fmt.Errorf("blokada %s: %w", name, err)
	}
	existing, err := leases.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return cluster.LeaseAttempt{}, fmt.Errorf("blokada %s: %w", name, err)
	}
	if existing.Spec.RenewTime != nil && existing.Spec.LeaseDurationSeconds != nil {
		expires := existing.Spec.RenewTime.Add(time.Duration(*existing.Spec.LeaseDurationSeconds) * time.Second)
		if time.Now().Before(expires) {
			other := "?"
			if existing.Spec.HolderIdentity != nil {
				other = *existing.Spec.HolderIdentity
			}
			return cluster.LeaseAttempt{Holder: other, Expires: expires}, nil
		}
	}
	existing.Spec = spec
	if _, err := leases.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return cluster.LeaseAttempt{Conflict: true}, nil
		}
		return cluster.LeaseAttempt{}, fmt.Errorf("blokada %s: %w", name, err)
	}
	return cluster.LeaseAttempt{Acquired: true, TookOver: true}, nil
}

// RenewLease odnawia blokadę, jeśli wciąż należy do holder. Konflikt
// wersji nie jest groźny — następne odnowienie weźmie aktualną.
func (c *Cluster) RenewLease(ctx context.Context, namespace, name, holder string) error {
	leases := c.core.CoordinationV1().Leases(namespace)
	lease, err := leases.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder {
		return nil
	}
	now := metav1.NewMicroTime(time.Now())
	lease.Spec.RenewTime = &now
	_, err = leases.Update(ctx, lease, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		return nil
	}
	return err
}

// ReleaseLease zwalnia blokadę, jeśli należy do holder — z warunkiem na
// resourceVersion, więc nie usunie blokady przejętej w międzyczasie.
func (c *Cluster) ReleaseLease(ctx context.Context, namespace, name, holder string) error {
	leases := c.core.CoordinationV1().Leases(namespace)
	lease, err := leases.Get(ctx, name, metav1.GetOptions{})
	if err == nil && lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == holder {
		err = leases.Delete(ctx, name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{ResourceVersion: &lease.ResourceVersion},
		})
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("zwolnienie blokady: %w", err)
	}
	return nil
}
