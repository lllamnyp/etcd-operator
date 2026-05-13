# Installation

How to deploy the operator into a cluster. Assumes you have admin on the target cluster (the operator installs cluster-scoped CRDs and ClusterRoles) and a registry you can push to.

For the operator's runtime behaviour see [concepts](concepts.md); for day-2 operations see [operations](operations.md).

## Prerequisites

| Requirement | Note |
|---|---|
| Kubernetes | 1.29+ required for the CRD's CEL validation rules (`quantity()` library stable in 1.29). 1.28 may work if `CRDValidationRatcheting` is enabled. |
| Default `StorageClass` | The sample manifests omit `storageClassName` and rely on the namespace's default. Override per-cluster if you need a specific class. |
| Go (build-from-source only) | 1.25+, matches `go.mod`'s `toolchain` directive. |
| Docker / buildx (build-from-source only) | For producing the operator image. The Dockerfile uses `golang:1.25.10` for the builder and `gcr.io/distroless/static:nonroot` for runtime. |

Workload-side: every etcd Pod runs as UID 65532 with `runAsNonRoot=true`, `allowPrivilegeEscalation=false`, all capabilities dropped, and `seccompProfile=RuntimeDefault`. The Pods comply with the `restricted` PodSecurity profile. If your cluster enforces a stricter policy, see `controllers/etcdmember_controller.go`'s `buildPod` for the exact security context the operator emits and adjust accordingly.

## Quick deploy

The repo's Makefile drives a complete install. From a checkout:

```sh
# 1. Install the CRDs cluster-wide.
make install

# 2. Build the operator image (or skip to a prebuilt registry tag).
make docker-build docker-push IMG=<your-registry>/etcd-operator:<tag>

# 3. Deploy the operator (creates the etcd-operator-system namespace,
#    ClusterRole/Binding, controller Deployment, metrics service).
make deploy IMG=<your-registry>/etcd-operator:<tag>
```

The cluster must be able to pull from `<your-registry>`. For local clusters (`kind` / `minikube` / `k3d`), either sideload the image (`kind load docker-image ...`) or push to an ephemeral registry the cluster can reach (e.g. `ttl.sh/<random>:1h`); otherwise the operator Deployment goes `ImagePullBackOff` with no clear hint from the operator side.

By default this lands in the `etcd-operator-system` namespace. The deployment name is `etcd-operator-controller-manager`. Verify:

```sh
kubectl get pod -n etcd-operator-system
kubectl logs -n etcd-operator-system deploy/etcd-operator-controller-manager \
  -c manager --tail=20
```

You should see the manager start lines and an empty work-queue (no `EtcdCluster` resources yet).

## Manual install (no Make)

If you don't want to invoke the Makefile (e.g. GitOps environments where `kustomize` is run by a controller in-cluster):

```sh
# CRDs
kubectl apply -f config/crd/bases/

# Operator + RBAC + Service, rendered by kustomize:
bin/kustomize-v5.6.0 build config/default | kubectl apply -f -
```

Override the image inline:

```sh
cd config/manager && bin/kustomize-v5.6.0 edit set image controller=<your-image>
cd ../.. && bin/kustomize-v5.6.0 build config/default | kubectl apply -f -
```

The `bin/kustomize-v*` binary is auto-downloaded by `make kustomize` (version pinned to `v5.6.0` in the Makefile); a system-installed `kustomize` works equally if you have one.

## Create your first cluster

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: lllamnyp.su/v1alpha2
kind: EtcdCluster
metadata:
  name: my-etcd
  namespace: default
spec:
  replicas: 3
  version: 3.5.17
  storage: 1Gi
EOF
```

Watch it form:

```sh
kubectl get etcdcluster.lllamnyp.su my-etcd -w
```

The operator bootstraps a single seed first, latches `clusterID`, then adds the remaining members one at a time as learners (with promotion). A 3-member cluster typically reaches `READY=3` in well under a minute on a healthy cluster.

To open a shell to one of the members:

```sh
POD=$(kubectl get pod -l etcd.lllamnyp.su/cluster=my-etcd \
  -o jsonpath='{.items[0].metadata.name}')
kubectl exec -it "$POD" -- etcdctl --endpoints=http://localhost:2379 \
  member list -w table
```

Don't hard-code Pod names — they carry a random suffix from `GenerateName` (e.g. `my-etcd-7xq2k`). The label selector is the stable handle.

### Memory-backed variant

For reconstructable workloads (e.g. a Kubernetes-in-Kubernetes apiserver whose state is GitOps-managed) you can opt the cluster onto a tmpfs `emptyDir` instead of a PVC:

```yaml
apiVersion: lllamnyp.su/v1alpha2
kind: EtcdCluster
metadata:
  name: my-mem-etcd
  namespace: default
spec:
  replicas: 3
  version: 3.5.17
  storage: 256Mi       # tmpfs SizeLimit per member
  storageMedium: Memory
```

This trades durability for speed: a Pod that loses its tmpfs (eviction, node failure) loses its data and the member is automatically replaced via `MemberRemove` + scale-up. **Don't use it as a general-purpose etcd backend** — see [docs/concepts.md](concepts.md#storage) and [docs/operations.md](operations.md#memory-backed-clusters) for the full trade-off, and the production hardening this PR doesn't ship yet ([#16](https://github.com/lllamnyp/etcd-operator/issues/16)). The apiserver rejects `replicas: 0` on memory clusters via the [CEL validation rules](concepts.md#apiserver-enforced-validation).

## Image versions

`spec.version` in an `EtcdCluster` becomes `quay.io/coreos/etcd:v<version>`. The image repository is hard-coded in `controllers/helpers.go:EtcdImage`. Override it by patching the operator image with your own registry/repo if you mirror etcd internally.

Tested etcd versions in CI: **3.5.17**. The operator's etcd client is v3.6.x (Go module) which is wire-compatible with 3.5.x server.

Operator's own toolchain (relevant when building from source):

| Component | Version |
|---|---|
| Go | 1.25.10 |
| controller-runtime | v0.21 |
| k8s.io/api, k8s.io/client-go | v0.33 |
| controller-gen | v0.18.0 |
| kustomize | v5.6.0 |
| etcd client (`go.etcd.io/etcd/client/v3`) | v3.6.11 |
| Kubebuilder layout | v4 |

All pinned in `go.mod`, `Dockerfile`, and `Makefile`.

## RBAC

The operator runs as a ClusterRole — it needs to watch `EtcdCluster` and `EtcdMember` across all namespaces, plus create/delete the per-member Pods, PVCs, and Services in each user namespace. The full role lives in `config/rbac/role.yaml` (regenerated from `+kubebuilder:rbac` markers — don't hand-edit).

Single-namespace scoping is not currently exposed: `main.go` does not wire a namespace flag into the manager's `Cache.DefaultNamespaces`, so the manager always watches all namespaces. Limiting RBAC alone (ClusterRole → Role) is not sufficient — the manager will still attempt list/watch across the cluster and the API server will deny it. Scoped deployment is a follow-up.

## Networking

The operator creates two Services per `EtcdCluster`:

- **`<cluster>`** — headless (`clusterIP: None`), `publishNotReadyAddresses: true`, selector `etcd.lllamnyp.su/cluster=<cluster>`, exposes **both 2379 (client) and 2380 (peer)**. Used by etcd for peer discovery and by the operator's own etcd client (which dials per-pod DNS `<member>.<cluster>.<ns>.svc:2379` resolved through this service). `publishNotReadyAddresses` is required for bootstrap: members during the initial join window aren't `Ready` yet but still need DNS entries to find each other.
- **`<cluster>-client`** — `ClusterIP`, exposes 2379 only. Intended for end-user client traffic (load-balanced across all pods backing the selector).

External access (NodePort / LoadBalancer / Ingress) isn't created automatically. If you need it, layer a separate Service or Ingress on top of `<cluster>-client`'s selector.

A specific routing pitfall: kube-apiserver pointed at the headless Service or the client `ClusterIP` will round-robin its etcd client across all reachable backends, including any current learner. Learners reject `MemberList` etc. with "rpc not supported for learner". The operator's *own* etcd client filters learners out (issue #12 fix in `memberEndpoints` / `discoverMemberID`), but you can't make kube-apiserver do the same. The pragmatic options for apiserver→etcd:

- Point at a single voter Pod's per-pod DNS name. Simple, fragile (Pod rescheduling).
- Point at a leader-aware proxy (etcd's own gRPC-proxy, or a sidecar). Robust, extra moving part.
- Accept occasional "rpc not supported for learner" errors during scale-up windows; kube-apiserver's own retry layer absorbs them.

This is outside the operator's scope but documented because operators ask.

## Teardown

```sh
# Remove individual clusters first — their finalizers will clean up etcd state.
kubectl delete etcdcluster.lllamnyp.su --all -A

# Remove the operator.
make undeploy

# Remove the CRDs (only after all EtcdClusters are gone; the CRDs have
# protected finalizers via the operator).
make uninstall
```

Deleting an `EtcdCluster` while it's running cascades through every owned resource: the operator's finalizer on each `EtcdMember` calls `MemberRemove` (when the cluster itself is also being deleted, the operator detects this and skips `MemberRemove` to avoid a deadlock — see `handleDeletion` in `controllers/etcdmember_controller.go`). Pods and PVCs are then GC'd via owner-refs.

If the operator is uninstalled while `EtcdCluster` resources still exist, they're stranded — the finalizers won't run because no controller is reading the queue. Recovery is to either re-install the operator, or `kubectl patch ... --type=merge -p '{"metadata":{"finalizers":null}}'` on each `EtcdMember` (manual, leaves PVCs and Pods in place — clean them up by label).

## Upgrades

For now, in-place operator upgrades work via `kubectl set image` on the operator Deployment, but in-place etcd version upgrades **do not** — changing `spec.version` on an existing `EtcdCluster` only affects newly-created members. See [What's not supported](../README.md#whats-not-supported-yet). The current recommended path for an etcd-version bump is:

1. Scale up by one to introduce a new-version member as a learner.
2. Scale down by one to evict an old-version member.
3. Repeat for each member.
4. Once all members are on the new version, edit `spec.version` so future scale-ups use it directly.

This is manual and slow. A native rolling upgrade is a tracked follow-up.

## Development

Out-of-cluster development run (against the current `$KUBECONFIG`):

```sh
make run
```

This builds and runs the operator binary on your laptop. It can reconcile `EtcdCluster` resources but **cannot dial etcd via in-cluster DNS** — `MemberList`/`MemberAdd`/`MemberRemove` will fail. Useful for testing reconcile loop logic against the apiserver, not for end-to-end testing. For e2e use the deploy flow above.
