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

	"k8s.io/apimachinery/pkg/api/resource"
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
