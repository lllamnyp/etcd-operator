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
- Scale to zero: setting `spec.replicas: 0` parks the cluster rather than removing it. The 1→0 transition Patches `spec.dormant=true` on the surviving `EtcdMember`; the member controller deletes the Pod and leaves the PVC owned by the EtcdMember. Setting `spec.replicas >= 1` later flips `spec.dormant=false` on the same member, the Pod comes back, and etcd resumes from its existing data dir with the same ClusterID and member ID. The `EtcdMember` CR is preserved across the pause — there is no Create-by-name on resume and no PVC reparenting. See "Scale to zero" below for the caveats.
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
| `conditions[]` | `Available`, `Progressing`, `Degraded`. `Available=False/Paused` is the steady state of a paused cluster (`spec.replicas: 0`). |

### EtcdMember spec (operator-managed)
`clusterName`, `version`, `storage`, `bootstrap` (bool), `initialCluster` (the `--initial-cluster` flag value), `clusterToken`, `dormant` (bool). Names are apiserver-assigned via `GenerateName="<cluster>-"`. All fields except `initialCluster` are populated at Create time; `initialCluster` is filled in by a follow-up Patch — see "Create-then-Patch" below. `dormant` is flipped to `true` by the cluster controller on a 1→0 scale-down to pause the member (the member controller then deletes the Pod and leaves the PVC); it is flipped back to `false` on the next scale-up to wake the member against its existing PVC. See "Scale to zero" below.

### Create-then-Patch

Because each member's `--initial-cluster` flag contains its own name and we don't know that name until the apiserver assigns it via `GenerateName`, the cluster controller drives every member through three steps in order:

1. **Create** the `EtcdMember` CR with `GenerateName`, an empty `spec.initialCluster`, and (for the seed) `spec.bootstrap=true`.
2. **MemberAddAsLearner** (skipped for the seed; skipped on scale-up too if the peer URL is already registered, i.e. crash recovery) using the assigned name's peer URL.
3. **Patch** `spec.initialCluster` from etcd's authoritative member list.

The member controller refuses to start a Pod while `spec.initialCluster` is empty, so a transient "pending" CR (between steps 1 and 3) never reaches the data plane. If a reconcile crashes between any two steps the next reconcile adopts the pending CR and resumes — see the cluster-controller comments for the exact recovery branches. Operators inspecting the cluster mid-reconcile may see CRs with empty `spec.initialCluster`; that is the intentional transient state, not corruption.

### Scale to zero

Setting `spec.replicas: 0` "pauses" the cluster rather than dismantling it. The 1→0 transition is handled differently from any other scale-down step:

1. **Cluster controller** (in `scaleDown`): when it sees `desired == 0 && len(running) == 1`, it Patches `spec.dormant=true` on the surviving `EtcdMember`. The CR is **not** deleted.
2. **Member controller** (in `Reconcile`): on the next reconcile of that member, it observes `spec.dormant=true` and runs `ensurePodAbsent` — deletes the Pod and clears `status.podName`. The PVC stays owned by the EtcdMember (same UID, same owner-ref) so nothing reparented and nothing cascade-deletes.
3. The cluster controller's replica accounting filters dormant members from `current`, so the paused cluster reports `current=0/desired=0` and surfaces `Available=False/Paused` rather than `QuorumHealthy`.

The reverse transition (`spec.replicas: 0 → >= 1`) is "wake":

1. **Cluster controller** (in `scaleUp`): if it finds a member with `spec.dormant=true` and `len(running)==0`, it Patches `spec.dormant=false` on the same member. No name lookup, no Create, no PVC reparenting.
2. **Member controller** (in `Reconcile`): on the next reconcile, `spec.dormant=false` so the dormant gate doesn't fire; the normal `ensurePVC` (finds the still-owned PVC) → `ensurePod` (recreates the Pod) path runs.
3. **Pod starts**, etcd reads the existing data dir, resumes with the **same ClusterID and member ID** as before the pause.

If further scale-up is requested (`spec.replicas >= 2`), it proceeds normally from the now-running single-member cluster via `MemberAddAsLearner` + `MemberPromote`.

**Caveats:**

- **`spec.replicas: 0` from scratch is also a valid pause state**, but there is nothing to preserve — no member was ever bootstrapped, so no PVC exists. The `Available=False/Paused` condition's message reflects this ("no data has been written"). Scaling up from such a cluster fires a fresh bootstrap.
- **Don't delete the dormant PVC.** If you remove `data-<dormant-member>` while paused, the wake step still flips `spec.dormant=false` and the member controller creates a new PVC and Pod. Etcd starts with `--initial-cluster-state=new` and an empty data dir, forming a *new* cluster with a *new* ClusterID. The operator's latched `status.clusterID` becomes stale. Recovery is to delete the EtcdCluster and recreate from scratch (you've lost the data either way).
- **Deleting the EtcdCluster while paused** cascades the EtcdMember (which owns the PVC) and the PVC with it. A subsequently recreated EtcdCluster of the same name is a fresh cluster.
- **Multi-member scale-to-zero is staged through 1.** Only the 1→0 step Patches `spec.dormant`; intermediate steps (5→4, 4→3, etc.) Delete the victim CR normally and the finalizer calls `MemberRemove`. There is no "freeze all 5 members" mode — the surviving data is always the last remaining member's.

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
- **Scale-down**: most-recently-created member is picked (`CreationTimestamp` DESC + name DESC tiebreak), graceful `MemberRemove` via finalizer, in-flight-deletion gate, peer-list fallback when `MemberID` was never populated. The 1→0 step is special: it Patches `spec.dormant=true` on the surviving member instead of deleting it.
- **Scale to zero / wake**: pause Patches `spec.dormant=true` on the surviving member; the member controller deletes the Pod and leaves the PVC owned by the EtcdMember. Wake Patches `spec.dormant=false`; the member controller recreates the Pod against the existing PVC. The CR is never deleted across the pause cycle, so there is no Create-by-fixed-name, no PVC reparenting, no orphan-adopt machinery.
- **Discovery**: seed anchored on `spec.bootstrap=true` (robust to list order), rejection of partial / unexpected responses, separate `WaitingForSeed` vs `ClusterUnreachable` signalling, no-churn on repeated errors.
- **Status**: no-churn at steady state on both CRs, `ObservedGeneration` populated, `BrokenMembers` count (stub predicate).
- **Member controller**: pod creation is gated on `spec.initialCluster` being set (with the finalizer added first so mid-flight deletes still trigger `MemberRemove`), ready-gating on `MemberID` populated, peer-URL fallback during the post-add propagation window, transient-apiserver-error propagation, PVC stale-owner refusal, Pod liveness shape (TCP-only, no quorum dependency).
- **Service drift**: reconciles owned fields (selector, ports, `publishNotReadyAddresses`) while preserving user-added labels, annotations, and extra ports.

## License

Apache 2.0. See `LICENSE`.
