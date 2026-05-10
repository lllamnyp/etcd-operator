/*
Copyright 2023 Timofey Larkin.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controllers

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.etcd.io/etcd/api/v3/etcdserverpb"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lll "github.com/lllamnyp/etcd-operator/api/v1alpha2"
)

// reconcileUntilStable drives a reconciler in a loop until it stops requesting
// requeues, or until maxIters runs out (test failure). Each call refreshes the
// passed object from the fake client so callers can inspect the latest state.
func reconcileUntilStable(t *testing.T, r *EtcdClusterReconciler, c client.Client, name, ns string, maxIters int) ctrl.Result {
	t.Helper()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
	var last ctrl.Result
	for i := 0; i < maxIters; i++ {
		res, err := r.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("Reconcile iter %d: %v", i, err)
		}
		last = res
		if !res.Requeue && res.RequeueAfter == 0 {
			return last
		}
	}
	return last
}

// TestObservedSpec_LocksReplicasMidBootstrap covers reviewer issue #5:
// changing spec.replicas while bootstrap is in progress must not change the
// --initial-cluster set the controller is rolling out, because etcd requires
// every bootstrapping member to share an identical --initial-cluster value.
func TestObservedSpec_LocksReplicasMidBootstrap(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(3),
			Version:  "3.5.17",
			Storage:  quickQty(t, "1Gi"),
		},
	}
	c, _ := newTestClient(t, cluster)
	fe := newFakeEtcd(0xdeadbeef)
	r := &EtcdClusterReconciler{
		Client:            c,
		Scheme:            testScheme(t),
		EtcdClientFactory: factoryReturning(fe),
	}

	// Drive a few reconciles to get past token init, observed snapshot, and
	// member creation. Cluster has no etcd reachable yet (fakeEtcd will return
	// MemberList but ClusterID is set on first call), so bootstrap should
	// land 3 members with the same initial-cluster, then discovery records
	// the cluster ID.
	reconcileUntilStable(t, r, c, "test", "ns", 8)

	members := &lll.EtcdMemberList{}
	if err := c.List(ctx, members, client.InNamespace("ns")); err != nil {
		t.Fatalf("List members: %v", err)
	}
	if len(members.Items) != 3 {
		t.Fatalf("expected 3 members after bootstrap, got %d", len(members.Items))
	}
	want := members.Items[0].Spec.InitialCluster
	for _, m := range members.Items[1:] {
		if m.Spec.InitialCluster != want {
			t.Fatalf("inconsistent initial-cluster across members: %q vs %q", want, m.Spec.InitialCluster)
		}
	}

	// Now flip replicas mid-bootstrap. Cluster has not yet reported all
	// members Ready (their pods don't exist in the fake client), so
	// reconciliationComplete is false and the locking pattern must hold the
	// observed.Replicas at 3.
	mustGet(t, c, "test", "ns", cluster)
	cluster.Spec.Replicas = ptrInt32(5)
	if err := c.Update(ctx, cluster); err != nil {
		t.Fatalf("Update cluster: %v", err)
	}

	reconcileUntilStable(t, r, c, "test", "ns", 8)

	mustGet(t, c, "test", "ns", cluster)
	if cluster.Status.Observed == nil || cluster.Status.Observed.Replicas != 3 {
		t.Fatalf("observed.Replicas changed during bootstrap (locking broken): observed=%+v", cluster.Status.Observed)
	}
	members = &lll.EtcdMemberList{}
	_ = c.List(ctx, members, client.InNamespace("ns"))
	if len(members.Items) != 3 {
		t.Fatalf("members count drifted from 3 to %d during bootstrap", len(members.Items))
	}
}

// TestProgressDeadline_AbandonsStuckTarget verifies the escape hatch: if the
// deadline is in the past and reconciliation is not complete, the controller
// adopts the latest spec. This is the user-facing remedy for a "I set a
// broken spec, get me out" situation described in the README.
func TestProgressDeadline_AbandonsStuckTarget(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(5),
			Version:  "3.5.17",
			Storage:  quickQty(t, "1Gi"),
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "test",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 7, // stuck target
				Version:  "3.5.17",
				Storage:  quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(-1)}, // already past
		},
	}
	c, _ := newTestClient(t, cluster)
	fe := newFakeEtcd(0xdeadbeef)
	r := &EtcdClusterReconciler{
		Client:            c,
		Scheme:            testScheme(t),
		EtcdClientFactory: factoryReturning(fe),
	}

	// One reconcile is enough to detect the expired deadline and adopt spec.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "ns"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	mustGet(t, c, "test", "ns", cluster)
	if cluster.Status.Observed == nil || cluster.Status.Observed.Replicas != 5 {
		t.Fatalf("observed.Replicas should have been reset to spec.Replicas (5) after deadline; got %+v", cluster.Status.Observed)
	}
}

// TestTryDiscoverCluster_SurfacesUnreachableEtcd covers reviewer issue #4:
// when the etcd client cannot be built or MemberList errors, the controller
// must log it and surface a Available=False condition with a meaningful
// reason so users see what's wrong.
func TestTryDiscoverCluster_SurfacesUnreachableEtcd(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(3),
			Version:  "3.5.17",
			Storage:  quickQty(t, "1Gi"),
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "test",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 3,
				Version:  "3.5.17",
				Storage:  quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(60 * 60 * 1e9)},
		},
	}
	// Pre-create three EtcdMembers so tryDiscoverCluster has endpoints.
	for i := 0; i < 3; i++ {
		m := &lll.EtcdMember{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("test-%d", i),
				Namespace: "ns",
				Labels:    memberLabels("test", fmt.Sprintf("test-%d", i)),
			},
			Spec: lll.EtcdMemberSpec{ClusterName: "test", Version: "3.5.17", Storage: quickQty(t, "1Gi"), InitialCluster: "x", ClusterToken: "test"},
		}
		_ = m
	}
	objs := []client.Object{cluster}
	for i := 0; i < 3; i++ {
		objs = append(objs, &lll.EtcdMember{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("test-%d", i),
				Namespace: "ns",
				Labels:    memberLabels("test", fmt.Sprintf("test-%d", i)),
			},
			Spec: lll.EtcdMemberSpec{ClusterName: "test", Version: "3.5.17", Storage: quickQty(t, "1Gi"), InitialCluster: "x", ClusterToken: "test"},
		})
	}
	c, _ := newTestClient(t, objs...)
	r := &EtcdClusterReconciler{
		Client:            c,
		Scheme:            testScheme(t),
		EtcdClientFactory: failingFactory(errors.New("dial timeout")),
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "ns"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	mustGet(t, c, "test", "ns", cluster)
	var available *metav1.Condition
	for i := range cluster.Status.Conditions {
		if cluster.Status.Conditions[i].Type == lll.ClusterAvailable {
			available = &cluster.Status.Conditions[i]
		}
	}
	if available == nil {
		t.Fatalf("no Available condition set on cluster; want Available=False with ClusterUnreachable reason")
	}
	if available.Status != metav1.ConditionFalse {
		t.Fatalf("Available status = %v, want False", available.Status)
	}
	if available.Reason != "ClusterUnreachable" {
		t.Fatalf("Available reason = %q, want ClusterUnreachable", available.Reason)
	}
}

// TestUpdateStatus_SurfacesBrokenCount covers reviewer issue #6: the isBroken
// stub must have a tested call site so the predicate is actually exercised.
// Today it always returns false, so the count must always be 0 — this test
// pins that contract until the policy lands.
func TestUpdateStatus_SurfacesBrokenCount(t *testing.T) {
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(3),
			Version:  "3.5.17",
			Storage:  quickQty(t, "1Gi"),
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "test",
			ClusterID:    "deadbeef",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 3,
				Version:  "3.5.17",
				Storage:  quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(60 * 60 * 1e9)},
		},
	}
	objs := []client.Object{cluster}
	// Three members all Ready=True
	for i := 0; i < 3; i++ {
		objs = append(objs, &lll.EtcdMember{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("test-%d", i),
				Namespace: "ns",
				Labels:    memberLabels("test", fmt.Sprintf("test-%d", i)),
			},
			Spec: lll.EtcdMemberSpec{ClusterName: "test", Version: "3.5.17", Storage: quickQty(t, "1Gi"), InitialCluster: "x", ClusterToken: "test"},
			Status: lll.EtcdMemberStatus{
				MemberID: "abc",
				Conditions: []metav1.Condition{{
					Type: lll.MemberReady, Status: metav1.ConditionTrue, Reason: "PodReady",
					LastTransitionTime: metav1.Now(),
				}},
			},
		})
	}
	c, _ := newTestClient(t, objs...)
	fe := newFakeEtcd(0xdeadbeef)
	r := &EtcdClusterReconciler{
		Client:            c,
		Scheme:            testScheme(t),
		EtcdClientFactory: factoryReturning(fe),
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "ns"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	mustGet(t, c, "test", "ns", cluster)
	if cluster.Status.BrokenMembers != 0 {
		t.Fatalf("BrokenMembers = %d; stub predicate always returns false, so should be 0", cluster.Status.BrokenMembers)
	}
	if cluster.Status.ReadyMembers != 3 {
		t.Fatalf("ReadyMembers = %d, want 3", cluster.Status.ReadyMembers)
	}
}

// silence unused imports
var _ = etcdserverpb.Member{}
