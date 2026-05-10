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
	"testing"

	"go.etcd.io/etcd/api/v3/etcdserverpb"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	lll "github.com/lllamnyp/etcd-operator/api/v1alpha2"
)

// TestRemoveMemberFromEtcd_FallbackByName covers reviewer issue #1: when a
// member's status.MemberID is empty (e.g. the pod never became Ready before
// scale-down), the finalizer must still find the etcd-side member by name and
// MemberRemove it. Without this, scale-up + immediate scale-down orphans the
// MemberAdd in etcd's member list.
func TestRemoveMemberFromEtcd_FallbackByName(t *testing.T) {
	ctx := context.Background()

	// Cluster has two existing members and the never-Ready new member.
	cluster := &lll.EtcdCluster{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"}}
	existing0 := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns", Labels: memberLabels("test", "test-0")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
		Status:     lll.EtcdMemberStatus{PodName: "test-0"},
	}
	existing1 := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-1", Namespace: "ns", Labels: memberLabels("test", "test-1")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
		Status:     lll.EtcdMemberStatus{PodName: "test-1"},
	}
	// test-2 was just MemberAdd'd to etcd but the pod never came up, so
	// MemberID is still empty on the CR.
	victim := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-2", Namespace: "ns",
			Labels: memberLabels("test", "test-2"),
			Finalizers: []string{MemberFinalizer},
		},
		Spec: lll.EtcdMemberSpec{ClusterName: "test"},
		// No MemberID, no PodName.
	}

	// Etcd reflects the situation: 3 members, with test-2 added by URL but
	// no Name yet (etcd populates Name only after the joiner reports in).
	const orphanID uint64 = 0xc0ffee
	fe := newFakeEtcd(0xdeadbeef,
		&etcdserverpb.Member{ID: 0xa01, Name: "test-0", PeerURLs: []string{peerURL("test-0", "test", "ns")}},
		&etcdserverpb.Member{ID: 0xa02, Name: "test-1", PeerURLs: []string{peerURL("test-1", "test", "ns")}},
		&etcdserverpb.Member{ID: orphanID, Name: "", PeerURLs: []string{peerURL("test-2", "test", "ns")}},
	)

	c, _ := newTestClient(t, cluster, existing0, existing1, victim)
	r := &EtcdMemberReconciler{
		Client:            c,
		Scheme:            testScheme(t),
		EtcdClientFactory: factoryReturning(fe),
	}

	if err := r.removeMemberFromEtcd(ctx, victim); err != nil {
		t.Fatalf("removeMemberFromEtcd: %v", err)
	}

	if len(fe.removeCalls) != 1 || fe.removeCalls[0] != orphanID {
		t.Fatalf("expected MemberRemove(0x%x); got %v", orphanID, fe.removeCalls)
	}
}

// TestRemoveMemberFromEtcd_NotFoundIsClean: if the member doesn't appear in
// etcd's list at all, treat it as already gone — no error. Otherwise, the
// finalizer would block forever waiting for an etcd-side state that never
// materialises.
func TestRemoveMemberFromEtcd_NotFoundIsClean(t *testing.T) {
	ctx := context.Background()

	cluster := &lll.EtcdCluster{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"}}
	existing0 := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns", Labels: memberLabels("test", "test-0")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
		Status:     lll.EtcdMemberStatus{PodName: "test-0"},
	}
	victim := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-9", Namespace: "ns", Labels: memberLabels("test", "test-9"), Finalizers: []string{MemberFinalizer}},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
	}

	fe := newFakeEtcd(0xdeadbeef,
		&etcdserverpb.Member{ID: 0xa01, Name: "test-0", PeerURLs: []string{peerURL("test-0", "test", "ns")}},
	)

	c, _ := newTestClient(t, cluster, existing0, victim)
	r := &EtcdMemberReconciler{
		Client:            c,
		Scheme:            testScheme(t),
		EtcdClientFactory: factoryReturning(fe),
	}

	if err := r.removeMemberFromEtcd(ctx, victim); err != nil {
		t.Fatalf("removeMemberFromEtcd should not error when member already gone: %v", err)
	}
	if len(fe.removeCalls) != 0 {
		t.Fatalf("expected no MemberRemove call, got %v", fe.removeCalls)
	}
}

// TestEnsurePVC_RefusesStaleOwner covers reviewer issue #2: a same-named PVC
// owned by a now-deleted EtcdMember (pending GC) must NOT be bound to the new
// EtcdMember of the same name. Reusing the prior data dir would crashloop the
// new pod (etcd sees a memberID the cluster has just removed).
func TestEnsurePVC_RefusesStaleOwner(t *testing.T) {
	ctx := context.Background()

	staleUID := types.UID("old-uid")
	freshUID := types.UID("fresh-uid")

	stalePVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-test-1",
			Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "lllamnyp.su/v1alpha2",
				Kind:       "EtcdMember",
				Name:       "test-1",
				UID:        staleUID,
			}},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}

	freshMember := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-1", Namespace: "ns", UID: freshUID, Labels: memberLabels("test", "test-1")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test", Version: "3.5.17", Storage: quickQty(t, "1Gi"), InitialCluster: "x", ClusterToken: "test"},
	}

	c, _ := newTestClient(t, freshMember, stalePVC)
	r := &EtcdMemberReconciler{Client: c, Scheme: testScheme(t)}

	err := r.ensurePVC(ctx, freshMember)
	if err == nil {
		t.Fatalf("ensurePVC should refuse to reuse a PVC owned by a stale EtcdMember")
	}
}

// TestEnsurePVC_AcceptsOwnPVC: when the existing PVC's owner ref UID matches
// the current EtcdMember (a normal restart-after-pod-delete situation), reuse
// is fine.
func TestEnsurePVC_AcceptsOwnPVC(t *testing.T) {
	ctx := context.Background()

	uid := types.UID("same-uid")
	ownPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-test-0",
			Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "lllamnyp.su/v1alpha2",
				Kind:       "EtcdMember",
				Name:       "test-0",
				UID:        uid,
			}},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}

	member := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns", UID: uid, Labels: memberLabels("test", "test-0")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test", Version: "3.5.17", Storage: quickQty(t, "1Gi"), InitialCluster: "x", ClusterToken: "test"},
	}

	c, _ := newTestClient(t, member, ownPVC)
	r := &EtcdMemberReconciler{Client: c, Scheme: testScheme(t)}

	if err := r.ensurePVC(ctx, member); err != nil {
		t.Fatalf("ensurePVC for own PVC: %v", err)
	}
	if member.Status.PVCName != "data-test-0" {
		t.Fatalf("PVCName not recorded: %q", member.Status.PVCName)
	}
}

// TestUpdateStatus_NoMemberIDKeepsReadyFalse covers reviewer issue #3: a pod
// that's PodReady but without a populated MemberID must not be reported as
// MemberReady=True. Otherwise the cluster controller can count it toward
// readyMembers and a deletion in this window leaves an etcd-side orphan.
func TestUpdateStatus_NoMemberIDKeepsReadyFalse(t *testing.T) {
	ctx := context.Background()

	member := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns", Labels: memberLabels("test", "test-0")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test", Version: "3.5.17", Storage: quickQty(t, "1Gi"), InitialCluster: "x", ClusterToken: "test"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{readyPodCondition()},
		},
	}

	c, _ := newTestClient(t, member, pod)
	// Etcd is reachable but it doesn't yet know about test-0 by name —
	// simulating the brief window between the pod becoming Ready and etcd
	// propagating the joiner's identity.
	fe := newFakeEtcd(0xdeadbeef) // empty members list
	r := &EtcdMemberReconciler{
		Client:            c,
		Scheme:            testScheme(t),
		EtcdClientFactory: factoryReturning(fe),
	}

	if _, err := r.updateStatus(ctx, member); err != nil {
		t.Fatalf("updateStatus: %v", err)
	}

	mustGet(t, c, "test-0", "ns", member)
	var ready *metav1.Condition
	for i := range member.Status.Conditions {
		if member.Status.Conditions[i].Type == lll.MemberReady {
			ready = &member.Status.Conditions[i]
		}
	}
	if ready == nil {
		t.Fatalf("no MemberReady condition")
	}
	if ready.Status != metav1.ConditionFalse {
		t.Fatalf("Ready=%v, want False (no memberID populated yet)", ready.Status)
	}
	if ready.Reason != "DiscoveringMemberID" {
		t.Fatalf("Reason=%q, want DiscoveringMemberID", ready.Reason)
	}
	if member.Status.MemberID != "" {
		t.Fatalf("MemberID populated unexpectedly: %q", member.Status.MemberID)
	}
}

// TestUpdateStatus_PopulatesMemberIDAndFlipsReady: the happy path — etcd
// knows about this member by name, we record the hex ID and flip Ready=True.
func TestUpdateStatus_PopulatesMemberIDAndFlipsReady(t *testing.T) {
	ctx := context.Background()

	member := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns", Labels: memberLabels("test", "test-0")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test", Version: "3.5.17", Storage: quickQty(t, "1Gi"), InitialCluster: "x", ClusterToken: "test"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{readyPodCondition()},
		},
	}

	c, _ := newTestClient(t, member, pod)
	const wantID uint64 = 0xae36f238164a08ad
	fe := newFakeEtcd(0xdeadbeef,
		&etcdserverpb.Member{ID: wantID, Name: "test-0", PeerURLs: []string{peerURL("test-0", "test", "ns")}},
	)
	r := &EtcdMemberReconciler{
		Client:            c,
		Scheme:            testScheme(t),
		EtcdClientFactory: factoryReturning(fe),
	}

	if _, err := r.updateStatus(ctx, member); err != nil {
		t.Fatalf("updateStatus: %v", err)
	}

	mustGet(t, c, "test-0", "ns", member)
	if member.Status.MemberID != "ae36f238164a08ad" {
		t.Fatalf("MemberID = %q, want ae36f238164a08ad", member.Status.MemberID)
	}
	var ready *metav1.Condition
	for i := range member.Status.Conditions {
		if member.Status.Conditions[i].Type == lll.MemberReady {
			ready = &member.Status.Conditions[i]
		}
	}
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %+v, want True", ready)
	}
}

// TestRemoveMemberFromEtcd_LastMemberIsNoOp: if no other members exist (the
// cluster is being torn down or this is genuinely the last member), the
// finalizer can't reach a peer to call MemberRemove. Don't block — return
// nil so the finalizer can clear and the resource gets GC'd.
func TestRemoveMemberFromEtcd_LastMemberIsNoOp(t *testing.T) {
	ctx := context.Background()

	cluster := &lll.EtcdCluster{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"}}
	victim := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-0", Namespace: "ns",
			Labels: memberLabels("test", "test-0"),
			Finalizers: []string{MemberFinalizer},
		},
		Spec:   lll.EtcdMemberSpec{ClusterName: "test"},
		Status: lll.EtcdMemberStatus{MemberID: "abc"},
	}

	c, _ := newTestClient(t, cluster, victim)
	r := &EtcdMemberReconciler{
		Client:            c,
		Scheme:            testScheme(t),
		EtcdClientFactory: factoryReturning(newFakeEtcd(0xdead)), // never reached
	}

	if err := r.removeMemberFromEtcd(ctx, victim); err != nil {
		t.Fatalf("removeMemberFromEtcd should be a no-op when no peers reachable; got %v", err)
	}
}

// TestRemoveMemberFromEtcd_FactoryError: if we can build no etcd client at
// all, the finalizer must surface the error and retry rather than silently
// removing the finalizer (which would leave the etcd-side member orphaned).
func TestRemoveMemberFromEtcd_FactoryError(t *testing.T) {
	ctx := context.Background()

	cluster := &lll.EtcdCluster{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"}}
	otherMember := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-1", Namespace: "ns", Labels: memberLabels("test", "test-1")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
		Status:     lll.EtcdMemberStatus{PodName: "test-1"},
	}
	victim := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns", Labels: memberLabels("test", "test-0"), Finalizers: []string{MemberFinalizer}},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
		Status:     lll.EtcdMemberStatus{MemberID: "abc"},
	}

	c, _ := newTestClient(t, cluster, otherMember, victim)
	r := &EtcdMemberReconciler{
		Client:            c,
		Scheme:            testScheme(t),
		EtcdClientFactory: failingFactory(errors.New("dial timeout")),
	}

	err := r.removeMemberFromEtcd(ctx, victim)
	if err == nil {
		t.Fatalf("expected error from removeMemberFromEtcd when factory fails")
	}
}

// TestUpdateStatus_PodNotReadyKeepsReadyFalse covers the symmetric case to
// #3: if the pod itself isn't Ready, MemberReady should be False with reason
// PodNotReady (and we should never even attempt MemberID discovery).
func TestUpdateStatus_PodNotReadyKeepsReadyFalse(t *testing.T) {
	ctx := context.Background()

	member := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns", Labels: memberLabels("test", "test-0")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test", Version: "3.5.17", Storage: quickQty(t, "1Gi"), InitialCluster: "x", ClusterToken: "test"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
			}},
		},
	}

	c, _ := newTestClient(t, member, pod)
	// Factory should never be called when pod isn't Ready; using a failing
	// factory asserts that.
	r := &EtcdMemberReconciler{
		Client:            c,
		Scheme:            testScheme(t),
		EtcdClientFactory: failingFactory(errors.New("must not be called")),
	}

	if _, err := r.updateStatus(ctx, member); err != nil {
		t.Fatalf("updateStatus: %v", err)
	}

	mustGet(t, c, "test-0", "ns", member)
	var ready *metav1.Condition
	for i := range member.Status.Conditions {
		if member.Status.Conditions[i].Type == lll.MemberReady {
			ready = &member.Status.Conditions[i]
		}
	}
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "PodNotReady" {
		t.Fatalf("Ready condition = %+v, want False/PodNotReady", ready)
	}
}

// silence unused imports
var _ = ctrl.Result{}
