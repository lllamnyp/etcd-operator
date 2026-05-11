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
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"

	corev1 "k8s.io/api/core/v1"
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

// TestBootstrap_CreatesSingleSeedMember covers the simplified bootstrap
// model: regardless of spec.replicas, bootstrap creates exactly one seed
// member (member-0) whose --initial-cluster lists only itself. Subsequent
// members join via MemberAdd once ClusterID is latched.
func TestBootstrap_CreatesSingleSeedMember(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(5),
			Version:  "3.5.17",
			Storage:  quickQty(t, "1Gi"),
		},
	}
	c, _ := newTestClient(t, cluster)
	fe := newFakeEtcd(0xdeadbeef)
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(fe)}

	// Run enough reconciles to land past token + observed snapshots, then
	// trigger bootstrap. Bootstrap requeues 5s; we stop iterating then.
	reconcileUntilStable(t, r, c, "test", "ns", 8)

	members := &lll.EtcdMemberList{}
	if err := c.List(ctx, members, client.InNamespace("ns")); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(members.Items) != 1 {
		t.Fatalf("bootstrap must create exactly one member; got %d (%v)", len(members.Items),
			func() []string {
				ns := make([]string, 0, len(members.Items))
				for _, m := range members.Items {
					ns = append(ns, m.Name)
				}
				return ns
			}())
	}
	seed := members.Items[0]
	if seed.Name != "test-0" {
		t.Fatalf("seed member should be test-0; got %q", seed.Name)
	}
	wantIC := buildInitialCluster([]string{"test-0"}, "test", "ns")
	if seed.Spec.InitialCluster != wantIC {
		t.Fatalf("seed initial-cluster mismatch: got %q want %q", seed.Spec.InitialCluster, wantIC)
	}
	if !seed.Spec.Bootstrap {
		t.Fatalf("seed member must be marked Bootstrap=true")
	}
}

// TestObservedSpec_LocksMidFlight pins the locking pattern's correctness
// outside the bootstrap flow specifically. With single-member bootstrap the
// initial-cluster-coordination race is gone, but the locking pattern still
// matters: observed.Replicas must not change while a reconcile is in
// progress (e.g. mid scale-up).
func TestObservedSpec_LocksMidFlight(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(3),
			Version:  "3.5.17",
			Storage:  quickQty(t, "1Gi"),
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "ns-test-x",
			ClusterID:    "deadbeef",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 3,
				Version:  "3.5.17",
				Storage:  quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(time.Hour)},
		},
	}
	// One member exists, two more pending — clearly mid scale-up.
	c, _ := newTestClient(t, cluster, &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns", Labels: memberLabels("test", "test-0")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test", Version: "3.5.17"},
		Status:     lll.EtcdMemberStatus{PodName: "test-0", MemberID: "abc", Conditions: []metav1.Condition{{Type: lll.MemberReady, Status: metav1.ConditionTrue, Reason: "PodReady", LastTransitionTime: metav1.Now()}}},
	})

	// User flips spec.replicas to 7 mid-flight.
	mustGet(t, c, "test", "ns", cluster)
	cluster.Spec.Replicas = ptrInt32(7)
	if err := c.Update(ctx, cluster); err != nil {
		t.Fatalf("Update: %v", err)
	}

	fe := newFakeEtcd(0xdeadbeef, &etcdserverpb.Member{ID: 0xa01, Name: "test-0", PeerURLs: []string{peerURL("test-0", "test", "ns")}})
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(fe)}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "ns"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	mustGet(t, c, "test", "ns", cluster)
	if cluster.Status.Observed.Replicas != 3 {
		t.Fatalf("observed.Replicas changed mid-flight (locking broken): %d", cluster.Status.Observed.Replicas)
	}
}

// TestDeadlineExceeded_BootstrapTerminal: an expired deadline before the
// cluster has formed (ClusterID is empty) is a hard terminal error. The
// operator must not adopt a new target — pods of the partially-bootstrapped
// members carry an --initial-cluster value that can't be changed in place,
// so creating new members with a different --initial-cluster would prevent
// the cluster from ever forming. Recovery is delete-and-recreate.
func TestDeadlineExceeded_BootstrapTerminal(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(5), // user already tried to "fix"
			Version:  "3.5.17",
			Storage:  quickQty(t, "1Gi"),
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "test",
			// ClusterID intentionally empty — bootstrap hasn't completed.
			Observed: &lll.ObservedClusterSpec{
				Replicas: 3,
				Version:  "3.5.17",
				Storage:  quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(-1)}, // expired
		},
	}
	c, _ := newTestClient(t, cluster)
	r := &EtcdClusterReconciler{
		Client:            c,
		Scheme:            testScheme(t),
		EtcdClientFactory: factoryReturning(newFakeEtcd(0xdeadbeef)),
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "ns"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	mustGet(t, c, "test", "ns", cluster)
	if cluster.Status.Observed == nil || cluster.Status.Observed.Replicas != 3 {
		t.Fatalf("observed must NOT change during bootstrap deadline-exceeded; got %+v", cluster.Status.Observed)
	}
	var avail *metav1.Condition
	for i := range cluster.Status.Conditions {
		if cluster.Status.Conditions[i].Type == lll.ClusterAvailable {
			avail = &cluster.Status.Conditions[i]
		}
	}
	if avail == nil || avail.Status != metav1.ConditionFalse || avail.Reason != "BootstrapFailed" {
		t.Fatalf("Available condition = %+v; want False/BootstrapFailed", avail)
	}
}

// TestDeadlineExceeded_SteadyStateWaitsForSpecUpdate: after bootstrap, an
// expired deadline with spec == observed parks the operator in a terminal
// state until the user updates spec. observed must not auto-pivot.
func TestDeadlineExceeded_SteadyStateWaitsForSpecUpdate(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(7),
			Version:  "3.5.17",
			Storage:  quickQty(t, "1Gi"),
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "test",
			ClusterID:    "deadbeef", // bootstrapped
			Observed: &lll.ObservedClusterSpec{
				Replicas: 7, // spec == observed, deadline still expired
				Version:  "3.5.17",
				Storage:  quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(-1)},
		},
	}
	c, _ := newTestClient(t, cluster)
	r := &EtcdClusterReconciler{
		Client:            c,
		Scheme:            testScheme(t),
		EtcdClientFactory: factoryReturning(newFakeEtcd(0xdeadbeef)),
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "ns"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	mustGet(t, c, "test", "ns", cluster)
	if cluster.Status.Observed.Replicas != 7 {
		t.Fatalf("observed must not change while spec == observed; got %+v", cluster.Status.Observed)
	}
	var avail *metav1.Condition
	for i := range cluster.Status.Conditions {
		if cluster.Status.Conditions[i].Type == lll.ClusterAvailable {
			avail = &cluster.Status.Conditions[i]
		}
	}
	if avail == nil || avail.Status != metav1.ConditionFalse || avail.Reason != "DeadlineExceeded" {
		t.Fatalf("Available condition = %+v; want False/DeadlineExceeded", avail)
	}
}

// TestDeadlineExceeded_SteadyStateRetriesOnSpecUpdate: after bootstrap, an
// expired deadline with spec != observed is read as user intervention. The
// operator snapshots the new spec into observed and resumes.
func TestDeadlineExceeded_SteadyStateRetriesOnSpecUpdate(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(5), // user just edited spec down
			Version:  "3.5.17",
			Storage:  quickQty(t, "1Gi"),
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "test",
			ClusterID:    "deadbeef",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 100, // failed scale-up target
				Version:  "3.5.17",
				Storage:  quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(-1)},
		},
	}
	c, _ := newTestClient(t, cluster)
	r := &EtcdClusterReconciler{
		Client:            c,
		Scheme:            testScheme(t),
		EtcdClientFactory: factoryReturning(newFakeEtcd(0xdeadbeef)),
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "ns"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	mustGet(t, c, "test", "ns", cluster)
	if cluster.Status.Observed.Replicas != 5 {
		t.Fatalf("observed should adopt new spec (5) after spec edit; got %+v", cluster.Status.Observed)
	}
	if cluster.Status.ProgressDeadline == nil {
		t.Fatalf("new deadline should have been set on retry")
	}
	var prog *metav1.Condition
	for i := range cluster.Status.Conditions {
		if cluster.Status.Conditions[i].Type == lll.ClusterProgressing {
			prog = &cluster.Status.Conditions[i]
		}
	}
	if prog == nil || prog.Status != metav1.ConditionTrue || prog.Reason != "RetryAfterDeadline" {
		t.Fatalf("Progressing condition = %+v; want True/RetryAfterDeadline", prog)
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

// TestScaleUp_IdempotentAfterCrash covers reviewer issue #1: if a previous
// reconcile crashed between MemberAdd succeeding and the EtcdMember CR being
// created, the next reconcile must NOT call MemberAdd again (etcd would
// reject the duplicate peerURL); it should detect the existing entry and
// proceed straight to creating the CR.
func TestScaleUp_IdempotentAfterCrash(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(4),
			Version:  "3.5.17",
			Storage:  quickQty(t, "1Gi"),
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "test",
			ClusterID:    "deadbeef",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 4,
				Version:  "3.5.17",
				Storage:  quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(time.Hour)},
		},
	}
	objs := []client.Object{cluster}
	for i := 0; i < 3; i++ {
		objs = append(objs, &lll.EtcdMember{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("test-%d", i),
				Namespace: "ns",
				Labels:    memberLabels("test", fmt.Sprintf("test-%d", i)),
			},
			Spec:   lll.EtcdMemberSpec{ClusterName: "test", Version: "3.5.17", Storage: quickQty(t, "1Gi"), InitialCluster: "x", ClusterToken: "test"},
			Status: lll.EtcdMemberStatus{PodName: fmt.Sprintf("test-%d", i)},
		})
	}
	c, _ := newTestClient(t, objs...)

	// Etcd already has 4 members — including test-3 from a partial scale-up.
	fe := newFakeEtcd(0xdeadbeef,
		&etcdserverpb.Member{ID: 0xa01, Name: "test-0", PeerURLs: []string{peerURL("test-0", "test", "ns")}},
		&etcdserverpb.Member{ID: 0xa02, Name: "test-1", PeerURLs: []string{peerURL("test-1", "test", "ns")}},
		&etcdserverpb.Member{ID: 0xa03, Name: "test-2", PeerURLs: []string{peerURL("test-2", "test", "ns")}},
		&etcdserverpb.Member{ID: 0xa04, Name: "", PeerURLs: []string{peerURL("test-3", "test", "ns")}},
	)

	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(fe)}

	// Drive the scale-up branch directly — Reconcile would handle services
	// + membership snapshots; we want to assert MemberAdd isn't re-called.
	memberList := &lll.EtcdMemberList{}
	if err := c.List(ctx, memberList); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, err := r.scaleUp(ctx, cluster, memberList.Items); err != nil {
		t.Fatalf("scaleUp: %v", err)
	}

	if len(fe.addCalls) != 0 {
		t.Fatalf("MemberAdd should not be called when peerURL already in etcd; got %v", fe.addCalls)
	}
	got := &lll.EtcdMember{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "test-3"}, got); err != nil {
		t.Fatalf("EtcdMember test-3 should have been created: %v", err)
	}
}

// TestBootstrap_Idempotent: if bootstrap re-runs (controller restart between
// Create and the next reconcile), it must not error or duplicate the seed
// member. The Create returns AlreadyExists which is swallowed.
func TestBootstrap_Idempotent(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "ns-test-x",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 3, Version: "3.5.17", Storage: quickQty(t, "1Gi"),
			},
		},
	}
	seedName := "test-0"
	pre := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: seedName, Namespace: "ns", Labels: memberLabels("test", seedName)},
		Spec: lll.EtcdMemberSpec{
			ClusterName: "test", Version: "3.5.17", Storage: quickQty(t, "1Gi"),
			Bootstrap: true, InitialCluster: buildInitialCluster([]string{seedName}, "test", "ns"),
			ClusterToken: "ns-test-x",
		},
	}
	c, _ := newTestClient(t, cluster, pre)
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(newFakeEtcd(0xdeadbeef))}

	if _, err := r.bootstrap(ctx, cluster); err != nil {
		t.Fatalf("bootstrap should be idempotent across restart: %v", err)
	}
	members := &lll.EtcdMemberList{}
	_ = c.List(ctx, members)
	if len(members.Items) != 1 {
		t.Fatalf("expected exactly one member, got %d", len(members.Items))
	}
}

// TestScaleUp_WaitsForInFlightDeletion is the symmetric case of #3: while
// one EtcdMember is being deleted (e.g. a stale member from external
// `kubectl delete`), scale-up must not interleave a MemberAdd against the
// same etcd cluster the finalizer is running MemberRemove against.
func TestScaleUp_WaitsForInFlightDeletion(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(5),
			Version:  "3.5.17",
			Storage:  quickQty(t, "1Gi"),
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "ns-test-x",
			ClusterID:    "deadbeef",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 5, Version: "3.5.17", Storage: quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(time.Hour)},
		},
	}
	objs := []client.Object{cluster}
	now := metav1.Now()
	// 4 healthy, 1 deleting.
	for i := 0; i < 5; i++ {
		m := &lll.EtcdMember{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("test-%d", i), Namespace: "ns",
				Labels: memberLabels("test", fmt.Sprintf("test-%d", i)),
			},
			Spec:   lll.EtcdMemberSpec{ClusterName: "test"},
			Status: lll.EtcdMemberStatus{PodName: fmt.Sprintf("test-%d", i), MemberID: "abc", Conditions: []metav1.Condition{{Type: lll.MemberReady, Status: metav1.ConditionTrue, Reason: "PodReady", LastTransitionTime: now}}},
		}
		if i == 4 {
			m.DeletionTimestamp = &now
			m.Finalizers = []string{MemberFinalizer}
		}
		objs = append(objs, m)
	}
	c, _ := newTestClient(t, objs...)
	fe := newFakeEtcd(0xdeadbeef)
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(fe)}

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "ns"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected RequeueAfter while waiting for in-flight deletion; got %+v", res)
	}
	if len(fe.addCalls) > 0 {
		t.Fatalf("MemberAdd should not be called while a deletion is in flight; got %v", fe.addCalls)
	}
}

// TestScaleDown_WaitsForInFlightDeletion covers reviewer issue #3: while one
// EtcdMember is being deleted (DeletionTimestamp set, finalizer pending), no
// further deletion may be issued. Concurrent finalizers running MemberRemove
// against stale snapshots of the etcd cluster can race past quorum.
func TestScaleDown_WaitsForInFlightDeletion(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(1),
			Version:  "3.5.17",
			Storage:  quickQty(t, "1Gi"),
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "test",
			ClusterID:    "deadbeef",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 1,
				Version:  "3.5.17",
				Storage:  quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(time.Hour)},
		},
	}
	objs := []client.Object{cluster}
	now := metav1.Now()
	// 5 EtcdMembers: test-0 through test-4. test-4 is being deleted.
	for i := 0; i < 5; i++ {
		m := &lll.EtcdMember{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("test-%d", i),
				Namespace: "ns",
				Labels:    memberLabels("test", fmt.Sprintf("test-%d", i)),
			},
			Spec:   lll.EtcdMemberSpec{ClusterName: "test", Version: "3.5.17", Storage: quickQty(t, "1Gi"), InitialCluster: "x", ClusterToken: "test"},
			Status: lll.EtcdMemberStatus{PodName: fmt.Sprintf("test-%d", i), MemberID: "abc", Conditions: []metav1.Condition{{Type: lll.MemberReady, Status: metav1.ConditionTrue, Reason: "PodReady", LastTransitionTime: now}}},
		}
		if i == 4 {
			m.DeletionTimestamp = &now
			m.Finalizers = []string{MemberFinalizer}
		}
		objs = append(objs, m)
	}
	c, _ := newTestClient(t, objs...)
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(newFakeEtcd(0xdeadbeef))}

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "ns"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected RequeueAfter while waiting for in-flight deletion; got %+v", res)
	}

	// Assert no other member got a DeletionTimestamp.
	memberList := &lll.EtcdMemberList{}
	_ = c.List(ctx, memberList)
	for _, m := range memberList.Items {
		if m.Name == "test-4" {
			continue
		}
		if !m.DeletionTimestamp.IsZero() {
			t.Fatalf("member %s was unexpectedly deleted while test-4 deletion in flight", m.Name)
		}
	}
}

// TestSetClusterCondition_PopulatesObservedGeneration covers reviewer issue
// #8: conditions must carry the cluster's current Generation so consumers
// can tell whether the condition reflects the latest spec.
func TestSetClusterCondition_PopulatesObservedGeneration(t *testing.T) {
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", Generation: 7},
	}
	setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionTrue, "TestReason", "test")
	if len(cluster.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(cluster.Status.Conditions))
	}
	if cluster.Status.Conditions[0].ObservedGeneration != 7 {
		t.Fatalf("ObservedGeneration = %d, want 7", cluster.Status.Conditions[0].ObservedGeneration)
	}
}

// TestSetClusterCondition_ReturnsFalseIfNoChange covers reviewer issue #5:
// the helper signals when nothing actually changed so callers can skip the
// Status().Update and avoid a write-storm in terminal state.
func TestSetClusterCondition_ReturnsFalseIfNoChange(t *testing.T) {
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", Generation: 1},
	}
	if !setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionTrue, "Reason", "msg") {
		t.Fatalf("first call should report changed=true")
	}
	if setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionTrue, "Reason", "msg") {
		t.Fatalf("second identical call should report changed=false")
	}
	if !setClusterCondition(cluster, lll.ClusterAvailable, metav1.ConditionTrue, "Reason", "different message") {
		t.Fatalf("call with different message should report changed=true")
	}
}

// TestTryDiscoverCluster_RejectsPartialMembership covers reviewer issue #2:
// the cluster ID must only latch when MemberList returns exactly one member
// matching the seed. A response with a different name (a spurious peer,
// stale cache, etc.) must not be anchored.
func TestTryDiscoverCluster_RejectsPartialMembership(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(3), Version: "3.5.17", Storage: quickQty(t, "1Gi"),
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "ns-test-x",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 3, Version: "3.5.17", Storage: quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(time.Hour)},
		},
	}
	seed := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns", Labels: memberLabels("test", "test-0")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
		Status:     lll.EtcdMemberStatus{PodName: "test-0"},
	}
	c, _ := newTestClient(t, cluster, seed)
	// fakeEtcd returns a member with the wrong name.
	fe := newFakeEtcd(0xdeadbeef, &etcdserverpb.Member{
		ID: 0xff, Name: "wrong-name", PeerURLs: []string{peerURL("wrong-name", "test", "ns")},
	})
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(fe)}

	memberList := &lll.EtcdMemberList{}
	_ = c.List(ctx, memberList, client.InNamespace("ns"))
	if _, err := r.tryDiscoverCluster(ctx, cluster, memberList.Items); err != nil {
		t.Fatalf("tryDiscoverCluster: %v", err)
	}
	mustGet(t, c, "test", "ns", cluster)
	if cluster.Status.ClusterID != "" {
		t.Fatalf("ClusterID should NOT be latched when MemberList returns an unexpected member; got %q", cluster.Status.ClusterID)
	}
}

// TestTryDiscoverCluster_LatchesOnValidResponse: with the right seed name
// and exactly one member returned, ClusterID is recorded in %016x form.
func TestTryDiscoverCluster_LatchesOnValidResponse(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(3), Version: "3.5.17", Storage: quickQty(t, "1Gi"),
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "ns-test-x",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 3, Version: "3.5.17", Storage: quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(time.Hour)},
		},
	}
	seed := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns", Labels: memberLabels("test", "test-0")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
		Status:     lll.EtcdMemberStatus{PodName: "test-0"},
	}
	c, _ := newTestClient(t, cluster, seed)
	fe := newFakeEtcd(0xabc, &etcdserverpb.Member{
		ID: 0xa01, Name: "test-0", PeerURLs: []string{peerURL("test-0", "test", "ns")},
	})
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(fe)}

	memberList := &lll.EtcdMemberList{}
	_ = c.List(ctx, memberList, client.InNamespace("ns"))
	if _, err := r.tryDiscoverCluster(ctx, cluster, memberList.Items); err != nil {
		t.Fatalf("tryDiscoverCluster: %v", err)
	}
	mustGet(t, c, "test", "ns", cluster)
	if cluster.Status.ClusterID != "0000000000000abc" {
		t.Fatalf("ClusterID should be zero-padded hex; got %q", cluster.Status.ClusterID)
	}
}

// TestDeadlineExceeded_NoChurnWhenSteady covers reviewer issue #5: while
// parked in a terminal state the controller must not write status on every
// reconcile.
func TestDeadlineExceeded_NoChurnWhenSteady(t *testing.T) {
	now := metav1.Now()
	past := metav1.NewTime(now.Add(-time.Hour))
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
				Replicas: 3, Version: "3.5.17", Storage: quickQty(t, "1Gi"),
			},
			ProgressDeadline: &past,
			Conditions: []metav1.Condition{
				{Type: lll.ClusterProgressing, Status: metav1.ConditionFalse, Reason: "BootstrapFailed", Message: "bootstrap deadline exceeded; delete the cluster and recreate to recover", LastTransitionTime: now},
				{Type: lll.ClusterAvailable, Status: metav1.ConditionFalse, Reason: "BootstrapFailed", Message: "could not bootstrap 3-member cluster within deadline", LastTransitionTime: now},
			},
		},
	}
	c, _ := newTestClient(t, cluster)
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(newFakeEtcd(0xdeadbeef))}

	rvBefore := mustGet(t, c, "test", "ns", &lll.EtcdCluster{}).ResourceVersion
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "ns"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 || res.Requeue {
		t.Fatalf("terminal state must return no requeue; got %+v", res)
	}
	rvAfter := mustGet(t, c, "test", "ns", &lll.EtcdCluster{}).ResourceVersion
	if rvBefore != rvAfter {
		t.Fatalf("ResourceVersion changed (%q -> %q) on no-op terminal reconcile", rvBefore, rvAfter)
	}
}

// TestTryDiscoverCluster_FindsSeedByName covers B2: even if the list order
// puts a non-seed member first, discovery must anchor to the seed (the
// member with name <cluster>-0). Otherwise a same-name slice from any
// future caller (or a user-created EtcdMember CR) would silently break
// discovery.
func TestTryDiscoverCluster_FindsSeedByName(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(3), Version: "3.5.17", Storage: quickQty(t, "1Gi"),
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "ns-test-x",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 3, Version: "3.5.17", Storage: quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(time.Hour)},
		},
	}
	// Pass members in an order where the seed is NOT first.
	test3 := lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-3", Namespace: "ns", Labels: memberLabels("test", "test-3")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
		Status:     lll.EtcdMemberStatus{PodName: "test-3"},
	}
	test0 := lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns", Labels: memberLabels("test", "test-0")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
		Status:     lll.EtcdMemberStatus{PodName: "test-0"},
	}
	c, _ := newTestClient(t, cluster, &test3, &test0)
	fe := newFakeEtcd(0xabc, &etcdserverpb.Member{
		ID: 0xa01, Name: "test-0", PeerURLs: []string{peerURL("test-0", "test", "ns")},
	})
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(fe)}

	if _, err := r.tryDiscoverCluster(ctx, cluster, []lll.EtcdMember{test3, test0}); err != nil {
		t.Fatalf("tryDiscoverCluster: %v", err)
	}
	mustGet(t, c, "test", "ns", cluster)
	if cluster.Status.ClusterID != "0000000000000abc" {
		t.Fatalf("ClusterID should be latched via seed lookup; got %q", cluster.Status.ClusterID)
	}
}

// TestTryDiscoverCluster_EmptySliceDoesNotPanic covers B3: a future caller
// passing an empty slice must not panic the controller.
func TestTryDiscoverCluster_EmptySliceDoesNotPanic(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "ns-test-x",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 3, Version: "3.5.17", Storage: quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(time.Hour)},
		},
	}
	c, _ := newTestClient(t, cluster)
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(newFakeEtcd(0xdead))}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("tryDiscoverCluster panicked on empty slice: %v", r)
		}
	}()
	res, err := r.tryDiscoverCluster(ctx, cluster, nil)
	if err != nil {
		t.Fatalf("tryDiscoverCluster: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected requeue when no seed available; got %+v", res)
	}
}

// TestEnsureService_ReconcilesOwnedFields covers B7: drift on the fields
// the operator owns (selector, our ports, publishNotReadyAddresses) is
// reconciled, but user-added labels/annotations and extra ports survive.
func TestEnsureService_ReconcilesOwnedFields(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
	}
	// Pre-create the headless Service with a broken selector and an extra
	// label/port that should survive reconciliation.
	preexisting := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test",
			Namespace:   "ns",
			Labels:      map[string]string{"injected-by-webhook": "yes"},
			Annotations: map[string]string{"some-webhook.io/managed": "true"},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: false, // wrong — drift to fix
			Selector:                 map[string]string{"unrelated": "wrong"},
			Ports: []corev1.ServicePort{
				// Extra user-added port — must survive.
				{Name: "metrics", Port: 9100, Protocol: corev1.ProtocolTCP},
			},
		},
	}
	c, _ := newTestClient(t, cluster, preexisting)
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(newFakeEtcd(0xdead))}

	if err := r.ensureService(ctx, cluster, "test", corev1.ServiceSpec{
		ClusterIP:                corev1.ClusterIPNone,
		PublishNotReadyAddresses: true,
		Selector:                 map[string]string{LabelCluster: "test"},
		Ports: []corev1.ServicePort{
			{Name: "client", Port: 2379},
			{Name: "peer", Port: 2380},
		},
	}); err != nil {
		t.Fatalf("ensureService: %v", err)
	}

	got := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "test"}, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.Selector[LabelCluster] != "test" {
		t.Fatalf("Selector not restored: %+v", got.Spec.Selector)
	}
	if !got.Spec.PublishNotReadyAddresses {
		t.Fatalf("PublishNotReadyAddresses not restored")
	}
	// User additions must survive.
	if got.Labels["injected-by-webhook"] != "yes" {
		t.Fatalf("user label was stripped: %+v", got.Labels)
	}
	if got.Annotations["some-webhook.io/managed"] != "true" {
		t.Fatalf("user annotation was stripped: %+v", got.Annotations)
	}
	foundMetrics, foundClient, foundPeer := false, false, false
	for _, p := range got.Spec.Ports {
		switch p.Name {
		case "metrics":
			foundMetrics = true
		case "client":
			foundClient = p.Port == 2379
		case "peer":
			foundPeer = p.Port == 2380
		}
	}
	if !foundMetrics {
		t.Fatalf("user-added metrics port was stripped: %+v", got.Spec.Ports)
	}
	if !foundClient || !foundPeer {
		t.Fatalf("required ports not present: %+v", got.Spec.Ports)
	}
}

// TestReconcile_FirstPassInitsTokenAndObserved covers B8: a brand-new
// cluster should have BOTH clusterToken AND observed populated after one
// Reconcile pass, not two.
func TestReconcile_FirstPassInitsTokenAndObserved(t *testing.T) {
	ctx := context.Background()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: types.UID("uid-1")},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(3), Version: "3.5.17", Storage: quickQty(t, "1Gi"),
		},
	}
	c, _ := newTestClient(t, cluster)
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(newFakeEtcd(0xdead))}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "ns"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	mustGet(t, c, "test", "ns", cluster)
	if cluster.Status.ClusterToken == "" {
		t.Fatalf("ClusterToken should be set after one reconcile pass")
	}
	if cluster.Status.Observed == nil {
		t.Fatalf("Observed should be set after one reconcile pass (combined with token init)")
	}
}

// TestScaleUp_WaitsForAllMembersReady covers blocker #1: scale-up must not
// fire MemberAddAsLearner while existing members are still NotReady. Doing
// so stacks joiners against a half-formed cluster.
func TestScaleUp_WaitsForAllMembersReady(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(5), Version: "3.5.17", Storage: quickQty(t, "1Gi"),
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "ns-test-x",
			ClusterID:    "deadbeef",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 5, Version: "3.5.17", Storage: quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(time.Hour)},
		},
	}
	objs := []client.Object{cluster}
	// test-0 Ready, test-1 and test-2 NotReady (still joining).
	for i := 0; i < 3; i++ {
		status := metav1.ConditionFalse
		if i == 0 {
			status = metav1.ConditionTrue
		}
		objs = append(objs, &lll.EtcdMember{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("test-%d", i), Namespace: "ns", Labels: memberLabels("test", fmt.Sprintf("test-%d", i))},
			Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
			Status: lll.EtcdMemberStatus{
				PodName:    fmt.Sprintf("test-%d", i),
				MemberID:   "abc",
				Conditions: []metav1.Condition{{Type: lll.MemberReady, Status: status, Reason: "x", LastTransitionTime: now}},
			},
		})
	}
	c, _ := newTestClient(t, objs...)
	fe := newFakeEtcd(0xdeadbeef)
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(fe)}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "ns"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(fe.addLearnerCalls) > 0 || len(fe.addCalls) > 0 {
		t.Fatalf("scale-up should not fire MemberAdd*/Promote while existing members are NotReady; got addCalls=%v addLearnerCalls=%v",
			fe.addCalls, fe.addLearnerCalls)
	}
}

// TestScaleUp_AddsAsLearner covers blocker #1 happy path: when current <
// desired AND existing members are all Ready, scale-up uses
// MemberAddAsLearner (not voting MemberAdd).
func TestScaleUp_AddsAsLearner(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "ns-test-x",
			ClusterID:    "deadbeef",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 3, Version: "3.5.17", Storage: quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(time.Hour)},
		},
	}
	objs := []client.Object{cluster}
	for i := 0; i < 2; i++ {
		objs = append(objs, &lll.EtcdMember{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("test-%d", i), Namespace: "ns", Labels: memberLabels("test", fmt.Sprintf("test-%d", i))},
			Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
			Status: lll.EtcdMemberStatus{
				PodName:    fmt.Sprintf("test-%d", i),
				MemberID:   "abc",
				Conditions: []metav1.Condition{{Type: lll.MemberReady, Status: metav1.ConditionTrue, Reason: "PodReady", LastTransitionTime: now}},
			},
		})
	}
	c, _ := newTestClient(t, objs...)
	fe := newFakeEtcd(0xdeadbeef,
		&etcdserverpb.Member{ID: 0xa01, Name: "test-0", PeerURLs: []string{peerURL("test-0", "test", "ns")}},
		&etcdserverpb.Member{ID: 0xa02, Name: "test-1", PeerURLs: []string{peerURL("test-1", "test", "ns")}},
	)
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(fe)}

	memberList := &lll.EtcdMemberList{}
	_ = c.List(ctx, memberList, client.InNamespace("ns"))
	if _, err := r.scaleUp(ctx, cluster, memberList.Items); err != nil {
		t.Fatalf("scaleUp: %v", err)
	}
	if len(fe.addCalls) > 0 {
		t.Fatalf("scale-up must use AddAsLearner, not voting MemberAdd; got addCalls=%v", fe.addCalls)
	}
	if len(fe.addLearnerCalls) != 1 || fe.addLearnerCalls[0] != peerURL("test-2", "test", "ns") {
		t.Fatalf("expected one MemberAddAsLearner against test-2 peer URL; got %v", fe.addLearnerCalls)
	}
}

// TestScaleUp_PromotesExistingLearner covers the second half of the learner
// flow: when etcd already has a learner (typical state on the iteration
// after MemberAddAsLearner + pod start), scale-up calls MemberPromote
// rather than adding another learner.
func TestScaleUp_PromotesExistingLearner(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "ns-test-x",
			ClusterID:    "deadbeef",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 3, Version: "3.5.17", Storage: quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(time.Hour)},
		},
	}
	objs := []client.Object{cluster}
	for i := 0; i < 2; i++ {
		objs = append(objs, &lll.EtcdMember{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("test-%d", i), Namespace: "ns", Labels: memberLabels("test", fmt.Sprintf("test-%d", i))},
			Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
			Status: lll.EtcdMemberStatus{
				PodName:    fmt.Sprintf("test-%d", i),
				MemberID:   "abc",
				Conditions: []metav1.Condition{{Type: lll.MemberReady, Status: metav1.ConditionTrue, Reason: "PodReady", LastTransitionTime: now}},
			},
		})
	}
	c, _ := newTestClient(t, objs...)
	const learnerID uint64 = 0xbb01
	fe := newFakeEtcd(0xdeadbeef,
		&etcdserverpb.Member{ID: 0xa01, Name: "test-0", PeerURLs: []string{peerURL("test-0", "test", "ns")}},
		&etcdserverpb.Member{ID: 0xa02, Name: "test-1", PeerURLs: []string{peerURL("test-1", "test", "ns")}},
		&etcdserverpb.Member{ID: learnerID, Name: "test-2", PeerURLs: []string{peerURL("test-2", "test", "ns")}, IsLearner: true},
	)
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(fe)}

	memberList := &lll.EtcdMemberList{}
	_ = c.List(ctx, memberList, client.InNamespace("ns"))
	if _, err := r.scaleUp(ctx, cluster, memberList.Items); err != nil {
		t.Fatalf("scaleUp: %v", err)
	}
	if len(fe.promoteCalls) != 1 || fe.promoteCalls[0] != learnerID {
		t.Fatalf("expected MemberPromote(%x); got %v", learnerID, fe.promoteCalls)
	}
	if len(fe.addLearnerCalls) > 0 {
		t.Fatalf("must not add another learner while one is pending promotion; got %v", fe.addLearnerCalls)
	}
}

// TestReconcile_PromotesLearnerWhenCurrentEqualsDesired covers a subtle
// fallout of the learner flow: the iteration that adds the *last* learner
// reaches current==desired before the learner is promoted. If scale-up
// stops being entered at that point, the final learner stays unpromoted
// forever. Reconcile's steady-state path must call promotePendingLearner.
func TestReconcile_PromotesLearnerWhenCurrentEqualsDesired(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(3), Version: "3.5.17", Storage: quickQty(t, "1Gi"),
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "ns-test-x",
			ClusterID:    "deadbeef",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 3, Version: "3.5.17", Storage: quickQty(t, "1Gi"),
			},
			ProgressDeadline: &metav1.Time{Time: metav1.Now().Add(time.Hour)},
		},
	}
	objs := []client.Object{cluster}
	for i := 0; i < 3; i++ {
		objs = append(objs, &lll.EtcdMember{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("test-%d", i), Namespace: "ns", Labels: memberLabels("test", fmt.Sprintf("test-%d", i))},
			Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
			Status: lll.EtcdMemberStatus{
				PodName:    fmt.Sprintf("test-%d", i),
				MemberID:   "abc",
				Conditions: []metav1.Condition{{Type: lll.MemberReady, Status: metav1.ConditionTrue, Reason: "PodReady", LastTransitionTime: now}},
			},
		})
	}
	c, _ := newTestClient(t, objs...)
	const learnerID uint64 = 0xbb01
	fe := newFakeEtcd(0xdeadbeef,
		&etcdserverpb.Member{ID: 0xa01, Name: "test-0", PeerURLs: []string{peerURL("test-0", "test", "ns")}},
		&etcdserverpb.Member{ID: 0xa02, Name: "test-1", PeerURLs: []string{peerURL("test-1", "test", "ns")}},
		&etcdserverpb.Member{ID: learnerID, Name: "test-2", PeerURLs: []string{peerURL("test-2", "test", "ns")}, IsLearner: true},
	)
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(fe)}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "ns"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(fe.promoteCalls) != 1 || fe.promoteCalls[0] != learnerID {
		t.Fatalf("expected MemberPromote(%x) at steady state; got %v", learnerID, fe.promoteCalls)
	}
}

// TestReconcile_ShortCircuitsOnDeletion covers blocker #3: when the cluster
// has a DeletionTimestamp, Reconcile must not run ensureServices.
func TestReconcile_ShortCircuitsOnDeletion(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test", Namespace: "ns",
			DeletionTimestamp: &now,
			Finalizers:        []string{"keep-me-alive"}, // fake client refuses to track delete-with-zero-finalizers
		},
		Spec: lll.EtcdClusterSpec{
			Replicas: ptrInt32(3), Version: "3.5.17", Storage: quickQty(t, "1Gi"),
		},
	}
	c, _ := newTestClient(t, cluster)
	r := &EtcdClusterReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(newFakeEtcd(0xdead))}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "ns"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	svcs := &corev1.ServiceList{}
	_ = c.List(ctx, svcs, client.InNamespace("ns"))
	if len(svcs.Items) != 0 {
		var names []string
		for _, s := range svcs.Items {
			names = append(names, s.Name)
		}
		t.Fatalf("Service should not be created for a Terminating cluster; got %v", names)
	}
}

// silence unused imports
var _ = etcdserverpb.Member{}
