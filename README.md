# etcd-operator

A Kubernetes operator for running [etcd](https://etcd.io/) clusters. Status: **early alpha** — API is `lllamnyp.su/v1alpha2` and will likely change.

## Overview

The operator manages etcd clusters via two custom resources:

- **`EtcdCluster`** — what the user creates. Captures cluster-wide intent: replica count, etcd version, per-member storage size, and a progress deadline. This is the only resource users normally touch.
- **`EtcdMember`** — what the operator creates. One per etcd member. Owns its own Pod and PVC. Users should not create or edit these directly; the cluster controller manages them.

There is **no StatefulSet**. Each member's Pod and PVC are reconciled independently by the member controller, which lets us model protocol-aware lifecycle (joining, member-id assignment, graceful removal) without fighting StatefulSet's "all replicas are one workload" assumption.

The cluster controller decides *which* members exist and orchestrates `MemberAddAsLearner` / `MemberPromote` / `MemberRemove` against the running etcd cluster. The member controller decides *how* a member becomes real — Pod, PVC, etcd flags — and reports observed facts (memberID, readiness) up to its CR's status.

Bootstrap is single-member: the operator creates one seed (an `EtcdMember` with `spec.bootstrap=true`) with `--initial-cluster-state=new` and `--initial-cluster` listing only itself. Once etcd reports that member's ID and the cluster ID, additional members are added one at a time via `MemberAddAsLearner` and then `MemberPromote` once each has caught up — voting quorum doesn't shift until promotion, so an unreachable joiner can never destabilise the existing cluster.

`EtcdMember` CRs are created with `ObjectMeta.GenerateName="<cluster>-"`, so each member's name is an apiserver-assigned random suffix (e.g. `mycluster-7xq2k`). Seed identity is anchored on `spec.bootstrap=true`, not on a predictable name; cross-incarnation safety (delete a cluster, create another of the same name) holds at every layer: the cluster ID is randomised by etcd, the `clusterToken` includes the EtcdCluster's UID, and member names diverge across incarnations.

## What's supported today

- Bootstrap of new clusters. The operator always creates exactly one seed member (`spec.bootstrap=true`) with `--initial-cluster-state=new`. After the seed is up and the cluster ID is latched, additional members are added one at a time as learners (`MemberAddAsLearner`) and promoted (`MemberPromote`) once etcd reports they've caught up, until `spec.replicas` is reached. Scale-up waits for every existing member to report `Ready=True` before starting the next step.
- Scale up: cluster controller Creates the new `EtcdMember` CR via `GenerateName`, then calls `MemberAddAsLearner` for the assigned name's peer URL, then Patches the CR's `spec.initialCluster` from etcd's authoritative member list. The member controller starts the Pod only once `spec.initialCluster` is set — see "Create-then-Patch" below.
- Scale down: cluster controller deletes the most-recently-created `EtcdMember` (`CreationTimestamp` DESC, name DESC tiebreak). There is no special seed-protection — etcd has no permanent "seed" role post-bootstrap, so any member can be removed. A finalizer on the deleted member calls `MemberRemove` against remaining peers before the Pod and PVC go away.
- Scale to zero: setting `spec.replicas: 0` parks the cluster rather than removing it. The 1→0 transition latches the surviving member's name in `status.dormantMember`, re-parents its PVC to the EtcdCluster (so cascade GC leaves it in place), and skips `MemberRemove` (so etcd's local data dir stays intact). Setting `spec.replicas >= 1` later resurrects the member with the same name; the parked PVC is adopted; etcd resumes from its existing data dir with the same ClusterID and member ID. See "Scale to zero" below for the data-loss caveats.
- Pod restart / node failure: data PVC is preserved, the new Pod reads the existing WAL and rejoins with the same member ID.
- Cluster deletion: cascading owner refs clean up everything; finalizers detect "the whole cluster is going away" and skip etcd-side removal to avoid deadlock.

## What's **not** supported (yet)

No TLS. No auth/RBAC inside etcd. No version upgrades — changing `spec.version` does not roll the new image through running pods (only new pods get the new version). No PVC resizing — changing `spec.storage` does not resize existing members' PVCs (only newly-created members on scale-up pick up the new size); see [#2](https://github.com/lllamnyp/etcd-operator/issues/2). No automatic broken-member replacement (the predicate is stubbed; `status.brokenMembers` always reads 0). No backups, no defragmentation scheduling, no PodAntiAffinity by default. See the [GitHub issue tracker](https://github.com/lllamnyp/etcd-operator/issues) for the running follow-up list.

## API at a glance

### EtcdCluster spec
| Field | Type | Default | Notes |
|---|---|---|---|
| `replicas` | `*int32` | `3` | Should be odd. Locked in via `status.observed.replicas` once a reconcile starts. |
| `version` | `string` | required | etcd version like `"3.5.17"`. Image becomes `quay.io/coreos/etcd:v<version>`. |
| `storage` | `Quantity` | `"1Gi"` | PVC size per member. Uses the namespace's default StorageClass. |
| `progressDeadlineSeconds` | `*int32` | `600` | How long the operator will spend reaching a target before abandoning it for the latest spec. |

### EtcdCluster status
| Field | Notes |
|---|---|
| `readyMembers` | Count of members that report `Ready=True`. |
| `brokenMembers` | Reserved for auto-replacement; currently always 0. |
| `clusterID` | Hex etcd cluster ID, set after bootstrap. |
| `clusterToken` | The `--initial-cluster-token` value the operator chose at bootstrap. Reused for all subsequent scale-up. Operator-managed; do not edit. The field is locked at first reconcile and persists across the entire cluster lifetime — recovery flows (deadline-exceeded, spec edits) never reset it. |
| `observed.{replicas,version,storage}` | The locked-in target the controller is currently reconciling toward. See "Locking pattern" below. |
| `progressDeadline` | Time at which the in-flight target will be abandoned. Patch this to a past time to abort a stuck reconcile. |
| `dormantMember` | Set while the cluster is paused at `spec.replicas: 0`. Holds the name of the last member that existed before the pause so a future scale-up resurrects the same member (preserving ClusterID and data). Empty in the normal running state. See "Scale to zero" below. |
| `conditions[]` | `Available`, `Progressing`, `Degraded`. |

### EtcdMember spec (operator-managed)
`clusterName`, `version`, `storage`, `bootstrap` (bool), `initialCluster` (the `--initial-cluster` flag value), `clusterToken`. Names are apiserver-assigned via `GenerateName="<cluster>-"`. All fields except `initialCluster` are populated at Create time; `initialCluster` is filled in by a follow-up Patch — see "Create-then-Patch" below.

### Create-then-Patch

Because each member's `--initial-cluster` flag contains its own name and we don't know that name until the apiserver assigns it via `GenerateName`, the cluster controller drives every member through three steps in order:

1. **Create** the `EtcdMember` CR with `GenerateName`, an empty `spec.initialCluster`, and (for the seed) `spec.bootstrap=true`.
2. **MemberAddAsLearner** (skipped for the seed; skipped on scale-up too if the peer URL is already registered, i.e. crash recovery) using the assigned name's peer URL.
3. **Patch** `spec.initialCluster` from etcd's authoritative member list.

The member controller refuses to start a Pod while `spec.initialCluster` is empty, so a transient "pending" CR (between steps 1 and 3) never reaches the data plane. If a reconcile crashes between any two steps the next reconcile adopts the pending CR and resumes — see the cluster-controller comments for the exact recovery branches. Operators inspecting the cluster mid-reconcile may see CRs with empty `spec.initialCluster`; that is the intentional transient state, not corruption.

### Scale to zero

Setting `spec.replicas: 0` "pauses" the cluster rather than dismantling it. The 1→0 transition is handled differently from any other scale-down step:

1. **Cluster controller** (in `scaleDown`): when it sees `desired == 0 && len(members) == 1`, it latches the surviving member's name in `status.dormantMember` and then deletes the `EtcdMember` CR.
2. **Member controller** (in the finalizer for that CR): notices the parent `EtcdCluster` has `status.observed.replicas == 0` and treats the deletion as a pause: re-parents the data PVC (`data-<name>`) so the EtcdCluster becomes its sole owner-controller, and skips `MemberRemove` so etcd's local data dir is left intact.
3. **Cascade GC** removes the EtcdMember and its Pod; the PVC stays — its only owner-controller is now the EtcdCluster, which still exists.

The reverse transition (`spec.replicas: 0 → >= 1`) is "resurrection":

1. **Cluster controller** (in `scaleUp`): if `status.dormantMember` is set and there are no live members, it re-creates an `EtcdMember` CR with that exact name (not `GenerateName`), `bootstrap: true`, and `spec.initialCluster` set to the single-member form. Then it clears `status.dormantMember`.
2. **Member controller** (in `ensurePVC`): sees a pre-existing PVC named `data-<name>` whose controller is the EtcdCluster, recognises the resurrection scenario, and re-parents the PVC back to the new EtcdMember.
3. **Pod starts**, etcd reads the existing data dir, resumes with the **same ClusterID and member ID** as before the pause.

If further scale-up is requested (`spec.replicas >= 2`), it proceeds normally from the now-running single-member cluster via `MemberAddAsLearner` + `MemberPromote`.

**Caveats:**

- **Don't delete the dormant PVC.** If you remove `data-<dormantMember>` while paused, resurrection still creates an `EtcdMember` of that name, but its etcd starts with an empty data dir and `--initial-cluster-state=new`, forming a *new* cluster with a *new* ClusterID. The operator's latched `status.clusterID` becomes stale. Recovery is to delete the EtcdCluster and recreate from scratch (you've lost the data either way).
- **No cross-incarnation portability.** If you delete the EtcdCluster itself while paused, cascade GC takes the PVC with it (the EtcdCluster is its only owner). A subsequently recreated EtcdCluster of the same name gets a fresh UID and cannot adopt the prior dormant PVC even if you preserved it manually — `tryAdoptFromCluster` checks the owner UID, not just the name.
- **Multi-member scale-to-zero is staged through 1.** The pause logic only fires on the 1→0 step; intermediate steps (5→4, 4→3, etc.) call `MemberRemove` normally. There is no "freeze all 5 members" mode — the surviving data is always the last remaining member's.

### EtcdMember status
`memberID` (hex), `podName`, `pvcName`, `conditions[]` (`Ready`).

## Locking pattern: how spec changes are absorbed

A naive operator would re-read `spec` every reconcile and start acting on every change. That breaks etcd in two well-known ways:

1. **Mid-bootstrap replica change** — etcd requires every bootstrapping member to start with an identical `--initial-cluster` flag. If the user edits `spec.replicas` between successive members being created, etcd refuses to form.
2. **Scale-up + immediate scale-down** — `MemberAdd` registers the new peer with etcd before its pod becomes Ready. If the user reverts `spec.replicas` in that window, a naive operator deletes a member it can't yet identify.

Both are the same underlying problem: the user's "desired state" is mutable on a faster cadence than the operator can converge.

**The fix:** the operator commits to a target. The first time a reconcile sees an EtcdCluster, it copies the spec into `status.observed` and sets `status.progressDeadline = now + spec.progressDeadlineSeconds`. From then on, **the controller reconciles against `status.observed`, not against `spec`**. Spec changes are noticed but not acted on — they only get adopted into `observed` when:

- The cluster has reached `observed` (i.e. the in-flight reconcile finished cleanly), or
- The deadline has passed (the in-flight reconcile gave up).

This is the same pattern Deployments use with `progressDeadlineSeconds`, applied at a coarser granularity.

### What happens when the deadline expires

An expired deadline is a **terminal error condition**. The operator stops acting on its own and waits for the user. There is no automatic pivot to the latest spec — silently moving on can corrupt a half-bootstrapped cluster, and in steady state it tends to widen the blast radius of stuck reconciles.

Recovery depends on whether the cluster ever finished bootstrapping.

**Before bootstrap finished** (`status.clusterID` is empty): the partially-created `EtcdMember` pods carry an `--initial-cluster` flag listing the original member set, and etcd will refuse to form once any subsequent member is started with a different value. There is no in-place fix. The condition will read:

```
Available: False, Reason=BootstrapFailed
```

The recovery is to delete the cluster and recreate. **Wait for the prior cluster's resources to fully GC before re-applying** — the operator refuses to reuse a PVC that's still owned by a now-deleted EtcdMember from the prior incarnation. Watch for these to disappear:

```sh
kubectl delete etcdcluster.lllamnyp.su my-cluster
# Wait until both queries below return nothing:
kubectl get etcdmember,pvc -l etcd.lllamnyp.su/cluster=my-cluster
kubectl apply -f my-cluster.yaml   # with the correct spec
```

**After bootstrap finished** (`status.clusterID` is set): the cluster itself is healthy; only the most recent operation got stuck (e.g. a scale-up to a replica count the cluster can't schedule). The operator parks with:

```
Available: False, Reason=DeadlineExceeded
Progressing: False, Reason=DeadlineExceeded
```

To recover, just edit `spec` to a sane value:

```sh
kubectl edit etcdcluster.lllamnyp.su my-cluster
```

The next reconcile notices `spec != status.observed`, treats the edit as your "I'm fixing this" signal, snapshots the new spec into `status.observed`, sets a fresh deadline, and resumes. (Until you edit spec, the operator just sits there with the terminal conditions.)

You can also force the deadline to expire early — patch `status.progressDeadline` to a past time — to escalate a slow reconcile to terminal state without waiting out the full window. Then follow whichever recovery path applies above.

> The locking is enforced by the controller, not by an admission webhook. A user can `kubectl edit` `spec` mid-flight and the API server will accept the change; the controller simply won't act on it until either the in-flight target is met, or the deadline expires and (in steady state) you've edited spec to something new.

## Quick start

### Install CRDs

```sh
make install
```

(or `kubectl apply -f config/crd/bases/`)

### Run the operator (out-of-cluster)

```sh
make run
```

Out-of-cluster runs work for development against the K8s API but **cannot dial etcd via in-cluster DNS** — `MemberList` / `MemberAdd` / `MemberRemove` will fail. For end-to-end testing, deploy the operator inside the cluster.

### Build and deploy in-cluster

```sh
make docker-build docker-push IMG=<registry>/etcd-operator:tag
make deploy IMG=<registry>/etcd-operator:tag
```

### Create a cluster

```sh
kubectl apply -f config/samples/_v1alpha2_etcdcluster.yaml
```

(or use `kubectl apply -k config/samples/`)

You should see three pods come up under default namespace, headless and client Services, and three PVCs. `kubectl get etcdcluster.lllamnyp.su` will eventually show `Ready=3` and a populated `clusterID`.

Member names are apiserver-assigned, so don't hard-code a pod name like `etcdcluster-sample-0`. Pick a member via the cluster label:

```sh
POD=$(kubectl get pod -l etcd.lllamnyp.su/cluster=etcdcluster-sample -o jsonpath='{.items[0].metadata.name}')
kubectl exec "$POD" -- etcdctl --endpoints=http://localhost:2379 endpoint health --cluster
```

should report all members healthy.

## Testing

Unit tests use a controller-runtime fake client and a fake etcd client (`controllers/testing_helpers_test.go`). Run:

```sh
go test ./controllers/...
```

The suite covers, roughly:

- **Bootstrap**: single-seed creation (via `GenerateName`, anchored on `spec.bootstrap=true`), idempotent recovery of a pending seed with empty `spec.initialCluster`, refusal to adopt a `Bootstrap=true` CR whose owner UID doesn't match this cluster.
- **Locking pattern**: `Observed`/`ProgressDeadline` mid-flight locking, bootstrap-deadline as terminal, steady-state-deadline waits for a spec edit before resuming.
- **Scale-up**: learner-mode (`MemberAddAsLearner`), readiness gate before the next step, both crash-recovery branches (CR Created but no AddAsLearner yet; AddAsLearner succeeded but `spec.initialCluster` not yet patched — including the "promote-can-never-succeed-before-patch" deadlock guard), `--initial-cluster` flag sourced from etcd's view, in-flight-deletion gate, post-final-add promotion.
- **Scale-down**: most-recently-created member is picked (`CreationTimestamp` DESC + name DESC tiebreak), graceful `MemberRemove` via finalizer, in-flight-deletion gate, peer-list fallback when `MemberID` was never populated. The 1→0 step is special: it latches `status.dormantMember`, re-parents the PVC to the EtcdCluster, and skips `MemberRemove` so resurrection can resume the same etcd state.
- **Scale to zero / resurrection**: pause latches `status.dormantMember` and re-parents the PVC; resume recreates the same-named `EtcdMember`, adopts the parked PVC (rejecting foreign-cluster owners), and clears `dormantMember`.
- **Discovery**: seed anchored on `spec.bootstrap=true` (robust to list order), rejection of partial / unexpected responses, separate `WaitingForSeed` vs `ClusterUnreachable` signalling, no-churn on repeated errors.
- **Status**: no-churn at steady state on both CRs, `ObservedGeneration` populated, `BrokenMembers` count (stub predicate).
- **Member controller**: pod creation is gated on `spec.initialCluster` being set (with the finalizer added first so mid-flight deletes still trigger `MemberRemove`), ready-gating on `MemberID` populated, peer-URL fallback during the post-add propagation window, transient-apiserver-error propagation, PVC stale-owner refusal, Pod liveness shape (TCP-only, no quorum dependency).
- **Service drift**: reconciles owned fields (selector, ports, `publishNotReadyAddresses`) while preserving user-added labels, annotations, and extra ports.

## License

Apache 2.0. See `LICENSE`.
