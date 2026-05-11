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
			Labels:     memberLabels("test", "test-2"),
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

// TestRemoveMemberFromEtcd_PeerWithEmptyPodNameRetries covers reviewer
// issue #3: if other members exist on the CR side but none have a PodName
// recorded yet (transient state, controller restart), removeMemberFromEtcd
// must NOT silently return nil — that would let the finalizer clear and
// orphan the etcd-side member. Return an error so we retry.
func TestRemoveMemberFromEtcd_PeerWithEmptyPodNameRetries(t *testing.T) {
	ctx := context.Background()

	cluster := &lll.EtcdCluster{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"}}
	// test-0 has no PodName recorded — simulating mid-bootstrap or
	// controller-restart staleness.
	other := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns", Labels: memberLabels("test", "test-0")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
		// Status.PodName intentionally empty.
	}
	victim := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-1", Namespace: "ns", Labels: memberLabels("test", "test-1"), Finalizers: []string{MemberFinalizer}},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
		Status:     lll.EtcdMemberStatus{MemberID: "abc"},
	}

	c, _ := newTestClient(t, cluster, other, victim)
	fe := newFakeEtcd(0xdeadbeef)
	r := &EtcdMemberReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(fe)}

	err := r.removeMemberFromEtcd(ctx, victim)
	if err == nil {
		t.Fatalf("expected error when peers exist on CR side but none have PodName set")
	}
	if len(fe.removeCalls) != 0 {
		t.Fatalf("MemberRemove should not be called when endpoints are empty; got %v", fe.removeCalls)
	}
}

// TestHandleDeletion_TransientGetErrorReturnsError covers reviewer issue
// #4: a non-NotFound error from getting the owner EtcdCluster must NOT be
// silently treated as "cluster alive" — that risks repeatedly firing
// MemberRemove against a cluster we can't actually introspect. Propagate
// the error so controller-runtime applies backoff.
func TestHandleDeletion_TransientGetErrorReturnsError(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	cluster := &lll.EtcdCluster{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"}}
	victim := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-0", Namespace: "ns",
			Labels:            memberLabels("test", "test-0"),
			Finalizers:        []string{MemberFinalizer},
			DeletionTimestamp: &now,
		},
		Spec:   lll.EtcdMemberSpec{ClusterName: "test"},
		Status: lll.EtcdMemberStatus{MemberID: "abc"},
	}
	base, _ := newTestClient(t, cluster, victim)
	c := &erroringGetClient{
		Client:     base,
		failOnKind: "EtcdCluster",
		err:        errors.New("apiserver flaked"),
	}
	r := &EtcdMemberReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(newFakeEtcd(0xdeadbeef))}

	if _, err := r.handleDeletion(ctx, victim); err == nil {
		t.Fatalf("expected error from handleDeletion on transient cluster Get error")
	}
	// Finalizer should still be in place — we didn't get a clean shutdown.
	mustGet(t, base, "test-0", "ns", victim)
	if !containsFinalizer(victim, MemberFinalizer) {
		t.Fatalf("finalizer was removed despite Get error")
	}
}

func containsFinalizer(m *lll.EtcdMember, name string) bool {
	for _, f := range m.Finalizers {
		if f == name {
			return true
		}
	}
	return false
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

// TestEnsurePVC_RefusesPVCWithNoOwnerRefs: a PVC with no owner refs is no
// longer "adopted" — the only legitimate adoption flow (operator-managed
// scale-to-zero hand-off) is tracked separately and will use explicit
// re-parenting. Until then, ensurePVC accepts only PVCs we created.
func TestEnsurePVC_RefusesPVCWithNoOwnerRefs(t *testing.T) {
	ctx := context.Background()
	prePVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-test-0",
			Namespace: "ns",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	member := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns", UID: types.UID("uid"), Labels: memberLabels("test", "test-0")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test", Version: "3.5.17", Storage: quickQty(t, "1Gi"), InitialCluster: "x", ClusterToken: "test"},
	}

	c, _ := newTestClient(t, member, prePVC)
	r := &EtcdMemberReconciler{Client: c, Scheme: testScheme(t)}

	if err := r.ensurePVC(ctx, member); err == nil {
		t.Fatalf("ensurePVC should refuse to adopt a PVC with no owner references")
	}
}

// TestEnsurePVC_RefusesPVCOwnedByOther: a PVC owned by some other resource
// (a leaked owner ref, a Pod, another operator's CR) must not be silently
// mounted by an etcd member. ensurePVC errors out so the user can untangle
// the conflict explicitly.
func TestEnsurePVC_RefusesPVCOwnedByOther(t *testing.T) {
	ctx := context.Background()
	otherPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-test-0",
			Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "Pod",
				Name:       "some-other-pod",
				UID:        types.UID("other-uid"),
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
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns", UID: types.UID("uid"), Labels: memberLabels("test", "test-0")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test", Version: "3.5.17", Storage: quickQty(t, "1Gi"), InitialCluster: "x", ClusterToken: "test"},
	}

	c, _ := newTestClient(t, member, otherPVC)
	r := &EtcdMemberReconciler{Client: c, Scheme: testScheme(t)}

	if err := r.ensurePVC(ctx, member); err == nil {
		t.Fatalf("ensurePVC should refuse to mount a PVC owned by something other than this EtcdMember")
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
			Labels:     memberLabels("test", "test-0"),
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

// TestBuildPod_LivenessIsNotQuorumAware covers B1: the liveness probe must
// not require quorum. A liveness HTTPGet on /health kills every member
// during a transient partition. The check is a TCP socket on the peer
// port — process-alive only.
func TestBuildPod_LivenessIsNotQuorumAware(t *testing.T) {
	r := &EtcdMemberReconciler{}
	pod := r.buildPod(&lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns"},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test", Version: "3.5.17"},
	})
	lp := pod.Spec.Containers[0].LivenessProbe
	if lp == nil {
		t.Fatalf("missing liveness probe entirely")
	}
	if lp.HTTPGet != nil {
		t.Fatalf("liveness probe must not use HTTPGet (would require quorum on /health); got HTTPGet=%+v", lp.HTTPGet)
	}
	if lp.TCPSocket == nil {
		t.Fatalf("liveness probe should use TCPSocket")
	}
	if lp.TCPSocket.Port.IntValue() != 2380 {
		t.Fatalf("liveness TCP port = %d, want 2380 (peer)", lp.TCPSocket.Port.IntValue())
	}
}

// TestRemoveMemberFromEtcd_SkipsDeletingPeers covers B4: when other members
// are themselves Terminating, removeMemberFromEtcd must not dial their
// (about-to-vanish) endpoints. Filter active members first.
func TestRemoveMemberFromEtcd_SkipsDeletingPeers(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()

	cluster := &lll.EtcdCluster{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"}}
	healthy := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns", Labels: memberLabels("test", "test-0")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
		Status:     lll.EtcdMemberStatus{PodName: "test-0"},
	}
	dying := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-2", Namespace: "ns", Labels: memberLabels("test", "test-2"),
			DeletionTimestamp: &now, Finalizers: []string{MemberFinalizer}},
		Spec:   lll.EtcdMemberSpec{ClusterName: "test"},
		Status: lll.EtcdMemberStatus{PodName: "test-2"},
	}
	victim := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-1", Namespace: "ns", Labels: memberLabels("test", "test-1"),
			Finalizers: []string{MemberFinalizer}},
		Spec:   lll.EtcdMemberSpec{ClusterName: "test"},
		Status: lll.EtcdMemberStatus{MemberID: "0000000000000002", PodName: "test-1"},
	}

	c, _ := newTestClient(t, cluster, healthy, dying, victim)

	// Record the endpoints the factory was called with.
	var seenEndpoints []string
	fe := newFakeEtcd(0xdeadbeef,
		&etcdserverpb.Member{ID: 0x1, Name: "test-0", PeerURLs: []string{peerURL("test-0", "test", "ns")}},
		&etcdserverpb.Member{ID: 0x2, Name: "test-1", PeerURLs: []string{peerURL("test-1", "test", "ns")}},
		&etcdserverpb.Member{ID: 0x3, Name: "test-2", PeerURLs: []string{peerURL("test-2", "test", "ns")}},
	)
	factory := func(_ context.Context, eps []string) (EtcdClusterClient, error) {
		seenEndpoints = append([]string(nil), eps...)
		return fe, nil
	}
	r := &EtcdMemberReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factory}

	if err := r.removeMemberFromEtcd(ctx, victim); err != nil {
		t.Fatalf("removeMemberFromEtcd: %v", err)
	}
	for _, ep := range seenEndpoints {
		if ep == clientURL("test-2", "test", "ns") {
			t.Fatalf("dialed a Terminating peer (test-2); endpoints were %v", seenEndpoints)
		}
	}
	if len(seenEndpoints) != 1 || seenEndpoints[0] != clientURL("test-0", "test", "ns") {
		t.Fatalf("expected dial only against test-0; got %v", seenEndpoints)
	}
}

// TestDiscoverMemberID_FallsBackToPeers covers B5: if the member's own pod
// is crashlooping, peer members still know its ID. discoverMemberID must
// dial peers, not just self.
func TestDiscoverMemberID_FallsBackToPeers(t *testing.T) {
	ctx := context.Background()

	cluster := &lll.EtcdCluster{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"}}
	peer := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns", Labels: memberLabels("test", "test-0")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
		Status:     lll.EtcdMemberStatus{PodName: "test-0", MemberID: "0000000000000001"},
	}
	target := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-1", Namespace: "ns", Labels: memberLabels("test", "test-1")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
	}

	c, _ := newTestClient(t, cluster, peer, target)

	// Factory inspects endpoints; if the first is self URL, error; if peer
	// URL is present, succeed with a fake that knows about target.
	const wantID uint64 = 0xfeedface
	fe := newFakeEtcd(0xdead,
		&etcdserverpb.Member{ID: 0x1, Name: "test-0", PeerURLs: []string{peerURL("test-0", "test", "ns")}},
		&etcdserverpb.Member{ID: wantID, Name: "test-1", PeerURLs: []string{peerURL("test-1", "test", "ns")}},
	)
	var capturedEndpoints []string
	factory := func(_ context.Context, eps []string) (EtcdClusterClient, error) {
		capturedEndpoints = append([]string(nil), eps...)
		return fe, nil
	}
	r := &EtcdMemberReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factory}

	id, err := r.discoverMemberID(ctx, target)
	if err != nil {
		t.Fatalf("discoverMemberID: %v", err)
	}
	if id != wantID {
		t.Fatalf("id = %x, want %x", id, wantID)
	}
	// Assert at least one peer URL is in the endpoint list (and not just self).
	wantPeer := clientURL("test-0", "test", "ns")
	hasPeer := false
	for _, ep := range capturedEndpoints {
		if ep == wantPeer {
			hasPeer = true
			break
		}
	}
	if !hasPeer {
		t.Fatalf("discoverMemberID must include peer endpoints; got %v", capturedEndpoints)
	}
}

// TestDiscoverMemberID_FallsBackToPeerURL covers blocker #2: in the window
// between MemberAddAsLearner and etcd propagating the joiner's Name, the
// only stable identifier we have is the peer URL. discoverMemberID must
// match on PeerURLs as well as Name, otherwise scale-up stalls.
func TestDiscoverMemberID_FallsBackToPeerURL(t *testing.T) {
	ctx := context.Background()

	cluster := &lll.EtcdCluster{ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"}}
	target := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-1", Namespace: "ns", Labels: memberLabels("test", "test-1")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
	}
	c, _ := newTestClient(t, cluster, target)

	const wantID uint64 = 0xfeedface
	// fakeEtcd returns the target with Name="" but matching PeerURLs.
	fe := newFakeEtcd(0xdead,
		&etcdserverpb.Member{ID: wantID, Name: "", PeerURLs: []string{peerURL("test-1", "test", "ns")}},
	)
	r := &EtcdMemberReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(fe)}

	id, err := r.discoverMemberID(ctx, target)
	if err != nil {
		t.Fatalf("discoverMemberID should match by peer URL; got %v", err)
	}
	if id != wantID {
		t.Fatalf("id = %x, want %x", id, wantID)
	}
}

// TestUpdateStatus_NoChurnInSteadyState covers blocker #4: when nothing has
// changed since the previous reconcile, updateStatus must NOT issue a
// Status update. Otherwise every 30s periodic reconcile bumps
// resourceVersion and fans out a watch event for no reason.
func TestUpdateStatus_NoChurnInSteadyState(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	member := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns", Labels: memberLabels("test", "test-0")},
		Spec:       lll.EtcdMemberSpec{ClusterName: "test"},
		Status: lll.EtcdMemberStatus{
			PodName:  "test-0",
			PVCName:  "data-test-0",
			MemberID: "0000000000000001",
			Conditions: []metav1.Condition{{
				Type: lll.MemberReady, Status: metav1.ConditionTrue, Reason: "PodReady",
				Message: "etcd member is ready", LastTransitionTime: now,
			}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "ns"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{readyPodCondition()},
		},
	}
	c, _ := newTestClient(t, member, pod)
	r := &EtcdMemberReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(newFakeEtcd(0xdead))}

	rvBefore := mustGet(t, c, "test-0", "ns", &lll.EtcdMember{}).ResourceVersion
	if _, err := r.updateStatus(ctx, member); err != nil {
		t.Fatalf("updateStatus: %v", err)
	}
	rvAfter := mustGet(t, c, "test-0", "ns", &lll.EtcdMember{}).ResourceVersion
	if rvBefore != rvAfter {
		t.Fatalf("ResourceVersion changed (%q -> %q) on a no-op updateStatus", rvBefore, rvAfter)
	}
}

// silence unused imports
var _ = ctrl.Result{}
