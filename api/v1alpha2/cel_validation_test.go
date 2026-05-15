/*
Copyright 2023 Timofey Larkin.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha2_test

import (
	"context"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	lll "github.com/lllamnyp/etcd-operator/api/v1alpha2"
)

// The four CEL XValidation rules on EtcdClusterSpec, end-to-end against
// a real apiserver via envtest. CEL validation is apiserver-side, so
// unit-testing against an in-process fake client cannot exercise these
// contracts — envtest is the right test seam.

func skipIfNoEnvtest(t *testing.T) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; envtest harness not initialized")
	}
}

func TestCEL_StorageMediumImmutable(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := validCluster("immut-medium")
	c.Spec.StorageMedium = lll.StorageMediumDefault
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatalf("Create initial PVC-backed cluster: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, c) })

	// Flip the medium — must be rejected.
	got := &lll.EtcdCluster{}
	if err := k8s.Get(ctx, ctrlclient.ObjectKeyFromObject(c), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Spec.StorageMedium = lll.StorageMediumMemory
	err := k8s.Update(ctx, got)
	if err == nil {
		t.Fatalf("apiserver accepted storageMedium flip; expected rejection")
	}
	if !strings.Contains(err.Error(), "spec.storageMedium is immutable") {
		t.Fatalf("error did not mention immutability: %v", err)
	}
}

func TestCEL_StorageMustBeNonZeroForMemoryOnCreate(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := validCluster("memstore-zero")
	c.Spec.StorageMedium = lll.StorageMediumMemory
	c.Spec.Storage = resource.MustParse("0")

	err := k8s.Create(ctx, c)
	if err == nil {
		_ = k8s.Delete(ctx, c)
		t.Fatalf("apiserver accepted storage=0 with medium=Memory; expected rejection")
	}
	if !strings.Contains(err.Error(), "spec.storage must be > 0") {
		t.Fatalf("error did not mention non-zero storage requirement: %v", err)
	}
}

func TestCEL_ReplicasZeroWithMemoryRejected(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	// Create-time rejection.
	c := validCluster("zero-mem-create")
	c.Spec.StorageMedium = lll.StorageMediumMemory
	c.Spec.Replicas = ptr32(0)

	err := k8s.Create(ctx, c)
	if err == nil {
		_ = k8s.Delete(ctx, c)
		t.Fatalf("apiserver accepted replicas=0 + Memory on Create; expected rejection")
	}
	if !strings.Contains(err.Error(), "replicas=0 with spec.storageMedium=Memory") {
		t.Fatalf("error did not mention replicas+Memory rejection on Create: %v", err)
	}

	// Update-time rejection: start at 3+Memory, scale to 0 → must be rejected.
	live := validCluster("zero-mem-update")
	live.Spec.StorageMedium = lll.StorageMediumMemory
	live.Spec.Replicas = ptr32(3)
	if err := k8s.Create(ctx, live); err != nil {
		t.Fatalf("Create baseline memory cluster: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, live) })

	got := &lll.EtcdCluster{}
	if err := k8s.Get(ctx, ctrlclient.ObjectKeyFromObject(live), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Spec.Replicas = ptr32(0)
	err = k8s.Update(ctx, got)
	if err == nil {
		t.Fatalf("apiserver accepted replicas: 3→0 on memory cluster; expected rejection")
	}
	if !strings.Contains(err.Error(), "replicas=0 with spec.storageMedium=Memory") {
		t.Fatalf("error did not mention replicas+Memory rejection on Update: %v", err)
	}
}

func TestCEL_StorageShrinkRejected(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := validCluster("storage-shrink")
	c.Spec.Storage = resource.MustParse("1Gi")
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, c) })

	got := &lll.EtcdCluster{}
	if err := k8s.Get(ctx, ctrlclient.ObjectKeyFromObject(c), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Spec.Storage = resource.MustParse("512Mi")

	err := k8s.Update(ctx, got)
	if err == nil {
		t.Fatalf("apiserver accepted storage shrink 1Gi→512Mi; expected rejection")
	}
	if !strings.Contains(err.Error(), "spec.storage cannot be shrunk") {
		t.Fatalf("error did not mention shrink rejection: %v", err)
	}

	// Growing is fine — sanity check that the rule only blocks shrink.
	if err := k8s.Get(ctx, ctrlclient.ObjectKeyFromObject(c), got); err != nil {
		t.Fatalf("Get for grow check: %v", err)
	}
	got.Spec.Storage = resource.MustParse("2Gi")
	if err := k8s.Update(ctx, got); err != nil {
		t.Fatalf("growing storage rejected unexpectedly: %v", err)
	}
}

// TestCEL_StorageMustBeNonZero_IntegerInput exercises the kubectl-style
// integer input path (`storage: 0` without quotes in YAML, which the
// apiserver receives as a JSON number rather than a string). The
// typed Go API always serializes Quantity as a string, so the
// TestCEL_StorageMustBeNonZeroForMemoryOnCreate test above can't reach
// this code path. CEL's `quantity()` function requires a string;
// without explicit string() coercion, an integer input trips a
// "no such overload" CEL runtime error instead of returning the
// intended human-readable message.
func TestCEL_StorageMustBeNonZero_IntegerInput(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "lllamnyp.su",
		Version: "v1alpha2",
		Kind:    "EtcdCluster",
	})
	u.SetName("memstore-zero-int")
	u.SetNamespace("default")
	u.Object["spec"] = map[string]any{
		"replicas":      int64(3),
		"version":       "3.5.17",
		"storage":       int64(0), // <-- the case we care about
		"storageMedium": "Memory",
	}

	err := k8s.Create(ctx, u)
	if err == nil {
		_ = k8s.Delete(ctx, u)
		t.Fatalf("apiserver accepted storage=0 (integer) with medium=Memory; expected rejection")
	}
	if !strings.Contains(err.Error(), "spec.storage must be > 0") {
		t.Fatalf("error did not surface the intended message (CEL coercion missing?): %v", err)
	}
}

// TestCEL_TLSAddOnExistingClusterRejected verifies that flipping a plaintext
// cluster to TLS post-create is rejected. Pointer-field immutability is
// enforced at the spec level via the explicit has(self.tls)==has(oldSelf.tls)
// rule rather than `self == oldSelf` on the field, because the latter only
// fires when both sides are populated.
func TestCEL_TLSAddOnExistingClusterRejected(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := validCluster("tls-add")
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatalf("Create plaintext cluster: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, c) })

	got := &lll.EtcdCluster{}
	if err := k8s.Get(ctx, ctrlclient.ObjectKeyFromObject(c), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Spec.TLS = &lll.EtcdClusterTLS{
		Client: &lll.ClientTLS{
			ServerSecretRef: corev1.LocalObjectReference{Name: "fake-server-tls"},
		},
	}

	err := k8s.Update(ctx, got)
	if err == nil {
		t.Fatalf("apiserver accepted TLS being added to existing plaintext cluster; expected rejection")
	}
	if !strings.Contains(err.Error(), "spec.tls cannot be added") {
		t.Fatalf("error did not mention add/remove rejection: %v", err)
	}
}

// TestCEL_TLSRemoveOnExistingClusterRejected mirrors the previous case for
// the TLS→plaintext direction.
func TestCEL_TLSRemoveOnExistingClusterRejected(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := validCluster("tls-remove")
	c.Spec.TLS = &lll.EtcdClusterTLS{
		Client: &lll.ClientTLS{
			ServerSecretRef: corev1.LocalObjectReference{Name: "fake-server-tls"},
		},
	}
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatalf("Create TLS cluster: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, c) })

	got := &lll.EtcdCluster{}
	if err := k8s.Get(ctx, ctrlclient.ObjectKeyFromObject(c), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Spec.TLS = nil

	err := k8s.Update(ctx, got)
	if err == nil {
		t.Fatalf("apiserver accepted TLS being removed from existing TLS cluster; expected rejection")
	}
	if !strings.Contains(err.Error(), "spec.tls cannot be added") {
		t.Fatalf("error did not mention add/remove rejection: %v", err)
	}
}

// TestCEL_TLSSubfieldChangeRejected verifies that the inner-secret-ref
// immutability rule fires when both sides have tls set but content differs.
// Toggling mTLS on/off post-create or swapping secret refs is the
// intended blocked path.
func TestCEL_TLSSubfieldChangeRejected(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	c := validCluster("tls-subfield")
	c.Spec.TLS = &lll.EtcdClusterTLS{
		Client: &lll.ClientTLS{
			ServerSecretRef: corev1.LocalObjectReference{Name: "fake-server-tls"},
		},
	}
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatalf("Create TLS cluster: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, c) })

	got := &lll.EtcdCluster{}
	if err := k8s.Get(ctx, ctrlclient.ObjectKeyFromObject(c), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Spec.TLS.Client.OperatorClientSecretRef = &corev1.LocalObjectReference{Name: "fake-op-client-tls"}

	err := k8s.Update(ctx, got)
	if err == nil {
		t.Fatalf("apiserver accepted mTLS toggle (added operatorClientSecretRef); expected rejection")
	}
	if !strings.Contains(err.Error(), "spec.tls is immutable") {
		t.Fatalf("error did not mention subtree immutability: %v", err)
	}
}

// TestCEL_HappyPathAccepts is a negative-side guard: a fully valid
// cluster spec must pass the apiserver. Catches accidental rule
// inversions and over-broad CEL expressions.
func TestCEL_HappyPathAccepts(t *testing.T) {
	skipIfNoEnvtest(t)
	ctx := context.Background()

	cases := []struct {
		name string
		mut  func(*lll.EtcdCluster)
	}{
		{
			name: "PVC default",
			mut:  func(c *lll.EtcdCluster) {},
		},
		{
			name: "memory with positive storage",
			mut: func(c *lll.EtcdCluster) {
				c.Spec.StorageMedium = lll.StorageMediumMemory
				c.Spec.Storage = resource.MustParse("256Mi")
			},
		},
		{
			name: "replicas zero with PVC backend",
			mut: func(c *lll.EtcdCluster) {
				c.Spec.Replicas = ptr32(0)
				// PVC default; the wedge rule only fires for Memory.
			},
		},
		{
			name: "tls full mTLS on create",
			mut: func(c *lll.EtcdCluster) {
				c.Spec.TLS = &lll.EtcdClusterTLS{
					Client: &lll.ClientTLS{
						ServerSecretRef:         corev1.LocalObjectReference{Name: "fake-server-tls"},
						OperatorClientSecretRef: &corev1.LocalObjectReference{Name: "fake-op-client-tls"},
					},
					Peer: &lll.PeerTLS{
						SecretRef: corev1.LocalObjectReference{Name: "fake-peer-tls"},
					},
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validCluster("happy-" + strings.ReplaceAll(strings.ToLower(tc.name), " ", "-"))
			tc.mut(c)
			if err := k8s.Create(ctx, c); err != nil {
				t.Fatalf("apiserver rejected valid spec: %v", err)
			}
			t.Cleanup(func() { _ = k8s.Delete(ctx, c) })
		})
	}
}
