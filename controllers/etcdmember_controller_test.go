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
			Resources: corev1.VolumeResourceRequirements{
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
			Resources: corev1.VolumeResourceRequirements{
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
			Resources: corev1.VolumeResourceRequirements{
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
			Resources: corev1.VolumeResourceRequirements{
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

// TestReconcile_WaitsForInitialClusterPatch covers the GenerateName flow's
// pending state: the cluster controller Creates an EtcdMember CR before
// it can fill Spec.InitialCluster (the assigned name is needed to
// register the peer URL with etcd first). Until the cluster controller
// follows up with that patch, the member controller must not start a
// pod — its etcd container would have no --initial-cluster value. The
// finalizer is added even in the pending state so a mid-flight delete
// still triggers MemberRemove cleanup.
func TestReconcile_WaitsForInitialClusterPatch(t *testing.T) {
	ctx := context.Background()
	pending := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pndng", Namespace: "ns", Labels: clusterLabels("test"),
		},
		Spec: lll.EtcdMemberSpec{
			ClusterName: "test", Version: "3.5.17", Storage: quickQty(t, "1Gi"),
			ClusterToken: "ns-test-x",
			// InitialCluster intentionally empty.
		},
	}
	c, _ := newTestClient(t, pending)
	r := &EtcdMemberReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(newFakeEtcd(0xdead))}

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-pndng", Namespace: "ns"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected RequeueAfter for pending member; got %+v", res)
	}
	// Finalizer must be in place even in the pending state.
	got := mustGet(t, c, "test-pndng", "ns", &lll.EtcdMember{})
	hasFinalizer := false
	for _, f := range got.Finalizers {
		if f == MemberFinalizer {
			hasFinalizer = true
			break
		}
	}
	if !hasFinalizer {
		t.Fatalf("MemberFinalizer must be added before the InitialCluster gate; got %v", got.Finalizers)
	}
	// No PVC or Pod must have been created.
	pvcs := &corev1.PersistentVolumeClaimList{}
	_ = c.List(ctx, pvcs)
	if len(pvcs.Items) != 0 {
		t.Fatalf("PVC should not be created while InitialCluster is empty; got %d", len(pvcs.Items))
	}
	pods := &corev1.PodList{}
	_ = c.List(ctx, pods)
	if len(pods.Items) != 0 {
		t.Fatalf("Pod should not be created while InitialCluster is empty; got %d", len(pods.Items))
	}
}

// TestHandleDeletion_ScaleToZeroReparentsPVCAndSkipsMemberRemove covers
// the pause half of scale-to-zero. When the EtcdCluster's
// status.observed.replicas is 0, deleting an EtcdMember must:
//   - Re-parent its data PVC to the EtcdCluster (so cascade GC doesn't
//     take it),
//   - Skip MemberRemove (we want etcd's local data dir intact for
//     resurrection).
//
// The same finalizer behaviour fires regardless of which member is
// being deleted — but the scaleDown path only enters scale-to-zero on
// the 1→0 step.
func TestHandleDeletion_ScaleToZeroReparentsPVCAndSkipsMemberRemove(t *testing.T) {
	ctx := context.Background()
	tru := true

	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test", Namespace: "ns", UID: types.UID("cluster-uid"),
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "ns-test-x",
			ClusterID:    "deadbeef",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 0, Version: "3.5.17", Storage: quickQty(t, "1Gi"),
			},
			// The cluster controller's scaleDown latches DormantMember on
			// the actual 1→0 step (len(members)==1). That latch is the
			// signal the finalizer keys off to take the pause path.
			DormantMember: "test-saved1",
		},
	}
	now := metav1.Now()
	member := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-saved1", Namespace: "ns", UID: types.UID("member-uid"),
			DeletionTimestamp: &now,
			Finalizers:        []string{MemberFinalizer},
			Labels:            memberLabels("test", "test-saved1"),
		},
		Spec: lll.EtcdMemberSpec{
			ClusterName: "test", Version: "3.5.17", Storage: quickQty(t, "1Gi"),
			InitialCluster: "x", ClusterToken: "ns-test-x", Bootstrap: true,
		},
		Status: lll.EtcdMemberStatus{MemberID: "0000000000000001", PodName: "test-saved1"},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "data-test-saved1", Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "lllamnyp.su/v1alpha2", Kind: "EtcdMember",
				Name: "test-saved1", UID: types.UID("member-uid"), Controller: &tru, BlockOwnerDeletion: &tru,
			}},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	c, _ := newTestClient(t, cluster, member, pvc)
	fe := newFakeEtcd(0xdeadbeef, &etcdserverpb.Member{
		ID: 0x1, Name: "test-saved1", PeerURLs: []string{peerURL("test-saved1", "test", "ns")},
	})
	r := &EtcdMemberReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(fe)}

	if _, err := r.handleDeletion(ctx, member); err != nil {
		t.Fatalf("handleDeletion: %v", err)
	}
	// MemberRemove must NOT have been called — etcd state must be intact.
	if len(fe.removeCalls) != 0 {
		t.Fatalf("MemberRemove should be skipped on scale-to-zero; got %v", fe.removeCalls)
	}
	// PVC's controller-owner must now be the EtcdCluster, not the EtcdMember.
	got := mustGet(t, c, "data-test-saved1", "ns", &corev1.PersistentVolumeClaim{})
	if !metav1.IsControlledBy(got, cluster) {
		t.Fatalf("PVC controller-owner = %v, want EtcdCluster", got.OwnerReferences)
	}
	for _, o := range got.OwnerReferences {
		if o.Kind == "EtcdMember" {
			t.Fatalf("PVC should no longer reference the EtcdMember as an owner; got %+v", got.OwnerReferences)
		}
	}
}

// TestHandleDeletion_IntermediateScaleDownStillCallsMemberRemove pins the
// fix for a subtle bug: when going N→0 the user has set spec.replicas=0
// and the locking pattern reflects that as status.observed.replicas=0
// for the duration of the multi-step descent. But only the FINAL step
// (1→0) is a "pause". Intermediate steps (3→2, 2→1) still need to call
// MemberRemove or etcd is left with phantom voting members the operator
// can no longer reach.
//
// The cluster controller distinguishes by only setting
// status.DormantMember when len(members)==1; the member-controller
// finalizer keys off that exact signal rather than on observed.replicas.
func TestHandleDeletion_IntermediateScaleDownStillCallsMemberRemove(t *testing.T) {
	ctx := context.Background()
	tru := true

	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test", Namespace: "ns", UID: types.UID("cluster-uid"),
		},
		Status: lll.EtcdClusterStatus{
			ClusterToken: "ns-test-x",
			ClusterID:    "deadbeef",
			Observed: &lll.ObservedClusterSpec{
				Replicas: 0, Version: "3.5.17", Storage: quickQty(t, "1Gi"),
			},
			// DormantMember intentionally empty — this is an intermediate
			// step (3→2 or 2→1), not the final 1→0.
		},
	}
	now := metav1.Now()
	survivor := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-keep1", Namespace: "ns", UID: types.UID("keep-uid"),
			Labels: memberLabels("test", "test-keep1"),
		},
		Spec:   lll.EtcdMemberSpec{ClusterName: "test", InitialCluster: "x", ClusterToken: "ns-test-x"},
		Status: lll.EtcdMemberStatus{PodName: "test-keep1", MemberID: "00000000000000a1"},
	}
	victim := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-gone1", Namespace: "ns", UID: types.UID("gone-uid"),
			DeletionTimestamp: &now,
			Finalizers:        []string{MemberFinalizer},
			Labels:            memberLabels("test", "test-gone1"),
		},
		Spec:   lll.EtcdMemberSpec{ClusterName: "test", InitialCluster: "x", ClusterToken: "ns-test-x"},
		Status: lll.EtcdMemberStatus{PodName: "test-gone1", MemberID: "00000000000000b2"},
	}
	victimPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "data-test-gone1", Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "lllamnyp.su/v1alpha2", Kind: "EtcdMember",
				Name: "test-gone1", UID: types.UID("gone-uid"), Controller: &tru, BlockOwnerDeletion: &tru,
			}},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	c, _ := newTestClient(t, cluster, survivor, victim, victimPVC)
	fe := newFakeEtcd(0xdeadbeef,
		&etcdserverpb.Member{ID: 0xa1, Name: "test-keep1", PeerURLs: []string{peerURL("test-keep1", "test", "ns")}},
		&etcdserverpb.Member{ID: 0xb2, Name: "test-gone1", PeerURLs: []string{peerURL("test-gone1", "test", "ns")}},
	)
	r := &EtcdMemberReconciler{Client: c, Scheme: testScheme(t), EtcdClientFactory: factoryReturning(fe)}

	if _, err := r.handleDeletion(ctx, victim); err != nil {
		t.Fatalf("handleDeletion: %v", err)
	}
	// MemberRemove MUST have fired — etcd's voter set must shrink.
	if len(fe.removeCalls) != 1 || fe.removeCalls[0] != 0xb2 {
		t.Fatalf("MemberRemove(0xb2) expected on intermediate scale-down; got %v", fe.removeCalls)
	}
	// PVC's controller-owner must remain the EtcdMember (its own pre-existing
	// owner ref). Cluster-reparenting is the pause path; we must not have
	// taken it.
	got := mustGet(t, c, "data-test-gone1", "ns", &corev1.PersistentVolumeClaim{})
	if metav1.IsControlledBy(got, cluster) {
		t.Fatalf("PVC must NOT be reparented to cluster on intermediate scale-down; got %+v", got.OwnerReferences)
	}
}

// TestEnsurePVC_AdoptsClusterOwnedOnResurrection covers the resume half
// of scale-to-zero. A PVC controlled by the EtcdCluster (parked at
// pause time) must be adopted by the fresh EtcdMember that carries the
// dormant name. The reparented PVC has its OwnerReferences replaced
// with a single ref to the new EtcdMember; the data dir is preserved.
func TestEnsurePVC_AdoptsClusterOwnedOnResurrection(t *testing.T) {
	ctx := context.Background()
	tru := true

	cluster := &lll.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: types.UID("cluster-uid")},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "data-test-saved1", Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "lllamnyp.su/v1alpha2", Kind: "EtcdCluster",
				Name: "test", UID: types.UID("cluster-uid"), Controller: &tru, BlockOwnerDeletion: &tru,
			}},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	member := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-saved1", Namespace: "ns", UID: types.UID("fresh-member-uid"),
			Labels: memberLabels("test", "test-saved1"),
		},
		Spec: lll.EtcdMemberSpec{
			ClusterName: "test", Version: "3.5.17", Storage: quickQty(t, "1Gi"),
			InitialCluster: "x", ClusterToken: "ns-test-x", Bootstrap: true,
		},
	}
	c, _ := newTestClient(t, cluster, member, pvc)
	r := &EtcdMemberReconciler{Client: c, Scheme: testScheme(t)}

	if err := r.ensurePVC(ctx, member); err != nil {
		t.Fatalf("ensurePVC must adopt cluster-owned PVC on resurrection: %v", err)
	}
	got := mustGet(t, c, "data-test-saved1", "ns", &corev1.PersistentVolumeClaim{})
	if !metav1.IsControlledBy(got, member) {
		t.Fatalf("PVC controller-owner = %v, want EtcdMember %q", got.OwnerReferences, member.Name)
	}
	for _, o := range got.OwnerReferences {
		if o.Kind == "EtcdCluster" {
			t.Fatalf("PVC should no longer reference the EtcdCluster as an owner; got %+v", got.OwnerReferences)
		}
	}
	if member.Status.PVCName != "data-test-saved1" {
		t.Fatalf("member.Status.PVCName = %q, want %q", member.Status.PVCName, "data-test-saved1")
	}
}

// TestEnsurePVC_RefusesForeignClusterOwnedPVC: defence-in-depth — a PVC
// controlled by a DIFFERENT EtcdCluster (same namespace, different
// cluster.Name or different UID — both indicate a stale or cross-cluster
// owner) must not be adopted as ours.
func TestEnsurePVC_RefusesForeignClusterOwnedPVC(t *testing.T) {
	ctx := context.Background()
	tru := true

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "data-test-saved1", Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "lllamnyp.su/v1alpha2", Kind: "EtcdCluster",
				Name: "other-cluster", UID: types.UID("other-uid"), Controller: &tru, BlockOwnerDeletion: &tru,
			}},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	member := &lll.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-saved1", Namespace: "ns", UID: types.UID("fresh-member-uid"),
			Labels: memberLabels("test", "test-saved1"),
		},
		Spec: lll.EtcdMemberSpec{ClusterName: "test", Version: "3.5.17", Storage: quickQty(t, "1Gi"), InitialCluster: "x", ClusterToken: "ns-test-x"},
	}
	c, _ := newTestClient(t, member, pvc)
	r := &EtcdMemberReconciler{Client: c, Scheme: testScheme(t)}

	if err := r.ensurePVC(ctx, member); err == nil {
		t.Fatalf("ensurePVC should refuse a PVC owned by a different EtcdCluster")
	}
}

// silence unused imports
var _ = ctrl.Result{}
