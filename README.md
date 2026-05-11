# etcd-operator

A Kubernetes operator for running [etcd](https://etcd.io/) clusters. Status: **early alpha** — API is `lllamnyp.su/v1alpha2` and will likely change.

## Overview

The operator manages etcd clusters via two custom resources:

- **`EtcdCluster`** — what the user creates. Captures cluster-wide intent: replica count, etcd version, per-member storage size, and a progress deadline. This is the only resource users normally touch.
- **`EtcdMember`** — what the operator creates. One per etcd member. Owns its own Pod and PVC. Users should not create or edit these directly; the cluster controller manages them.

There is **no StatefulSet**. Each member's Pod and PVC are reconciled independently by the member controller, which lets us model protocol-aware lifecycle (joining, member-id assignment, graceful removal) without fighting StatefulSet's "all replicas are one workload" assumption.

The cluster controller decides *which* members exist and orchestrates `MemberAddAsLearner` / `MemberPromote` / `MemberRemove` against the running etcd cluster. The member controller decides *how* a member becomes real — Pod, PVC, etcd flags — and reports observed facts (memberID, readiness) up to its CR's status.

Bootstrap is single-member: the operator creates one seed (`<cluster>-0`) with `--initial-cluster-state=new` and `--initial-cluster` listing only itself. Once etcd reports that member's ID and the cluster ID, additional members are added one at a time via `MemberAddAsLearner` and then `MemberPromote` once each has caught up — voting quorum doesn't shift until promotion, so an unreachable joiner can never destabilise the existing cluster.

## What's supported today

- Bootstrap of new clusters. The operator always creates exactly one seed member (`<cluster>-0`) with `--initial-cluster-state=new`. After the seed is up and the cluster ID is latched, additional members are added one at a time as learners (`MemberAddAsLearner`) and promoted (`MemberPromote`) once etcd reports they've caught up, until `spec.replicas` is reached. Scale-up waits for every existing member to report `Ready=True` before starting the next step.
- Scale up: cluster controller calls `MemberAddAsLearner`, creates an `EtcdMember` whose pod joins as a learner with `--initial-cluster-state=existing`, then promotes the learner once etcd allows it.
- Scale down: cluster controller deletes the highest-ordinal `EtcdMember`. A finalizer on the deleted member calls `MemberRemove` against remaining peers before the Pod and PVC go away.
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
| `clusterToken` | The `--initial-cluster-token` value the operator chose at bootstrap. Reused for all subsequent scale-up. |
| `observed.{replicas,version,storage}` | The locked-in target the controller is currently reconciling toward. See "Locking pattern" below. |
| `progressDeadline` | Time at which the in-flight target will be abandoned. Patch this to a past time to abort a stuck reconcile. |
| `conditions[]` | `Available`, `Progressing`, `Degraded`. |

### EtcdMember spec (operator-managed)
`clusterName`, `version`, `storage`, `bootstrap` (bool), `initialCluster` (the `--initial-cluster` flag value), `clusterToken`. All copied from the cluster at creation time so the member controller is self-contained.

### EtcdMember status
`memberID` (hex), `podName`, `pvcName`, `conditions[]` (`Ready`).

## Locking pattern: how spec changes are absorbed

A naive operator would re-read `spec` every reconcile and start acting on every change. That breaks etcd in two well-known ways:

1. **Mid-bootstrap replica change** — etcd requires every bootstrapping member to start with an identical `--initial-cluster` flag. If the user edits `spec.replicas` between member-0 and member-2 being created, etcd refuses to form.
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

```sh
kubectl exec etcdcluster-sample-0 -- etcdctl --endpoints=http://localhost:2379 endpoint health --cluster
```

should report all members healthy.

## Testing

Unit tests use a controller-runtime fake client and a fake etcd client (`controllers/testing_helpers_test.go`). Run:

```sh
go test ./controllers/...
```

The suite covers, roughly:

- **Bootstrap**: single-seed creation, idempotent recovery, refusal to adopt a same-named seed from a deleted prior cluster.
- **Locking pattern**: `Observed`/`ProgressDeadline` mid-flight locking, bootstrap-deadline as terminal, steady-state-deadline waits for a spec edit before resuming.
- **Scale-up**: learner-mode (`MemberAddAsLearner`), readiness gate before the next step, crash-recovery when a learner is already in etcd but the CR is missing, `--initial-cluster` flag sourced from etcd's view, in-flight-deletion gate, post-final-add promotion.
- **Scale-down**: graceful `MemberRemove` via finalizer, in-flight-deletion gate, peer-list fallback when `MemberID` was never populated.
- **Discovery**: rejection of partial / unexpected responses, separate `WaitingForSeed` vs `ClusterUnreachable` signalling, no-churn on repeated errors.
- **Status**: no-churn at steady state on both CRs, `ObservedGeneration` populated, `BrokenMembers` count (stub predicate).
- **Member controller**: ready-gating on `MemberID` populated, peer-URL fallback during the post-add propagation window, transient-apiserver-error propagation, PVC stale-owner refusal, Pod liveness shape (TCP-only, no quorum dependency).
- **Service drift**: reconciles owned fields (selector, ports, `publishNotReadyAddresses`) while preserving user-added labels, annotations, and extra ports.

## License

Apache 2.0. See `LICENSE`.
