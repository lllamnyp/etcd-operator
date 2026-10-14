# etcd-operator has moved to [cozystack/etcd-operator](https://github.com/cozystack/etcd-operator)

> [!IMPORTANT]
> **This repository is retired and read-only.** Development continues at
> **<https://github.com/cozystack/etcd-operator>**. Open issues, pull requests,
> releases, the Helm chart and the container image all live there now.

## What happened to the work here

Nothing was lost. This repository's `master` branch, up to and including
[#26](https://github.com/lllamnyp/etcd-operator/pull/26) (commit `9a6cc65`), is
part of the `main` branch history of
[cozystack/etcd-operator](https://github.com/cozystack/etcd-operator) unchanged,
so every feature built here (learner-mode scaling, scale to zero, memory-backed
storage, CEL validation, PodDisruptionBudget, TLS via Secrets or cert-manager,
`spec.resources` and the `/scale` subresource) ships in the Cozystack releases.
Development continued there from the next commit onward.

The issues filed here are kept for reference. File new issues and pull
requests at
[cozystack/etcd-operator](https://github.com/cozystack/etcd-operator/issues).

## Names that changed in the move

| What | Here (retired) | Now |
|---|---|---|
| Repository | `github.com/lllamnyp/etcd-operator` | [`github.com/cozystack/etcd-operator`](https://github.com/cozystack/etcd-operator) |
| Go module | `github.com/lllamnyp/etcd-operator` | `github.com/cozystack/etcd-operator` |
| API group | `lllamnyp.su/v1alpha2` | `etcd-operator.cozystack.io/v1alpha2` |
| Cluster label | `etcd.lllamnyp.su/cluster` | `etcd-operator.cozystack.io/cluster` |
| Operator image | `docker.io/lllamnyp/etcd-operator` (no new builds) | `ghcr.io/cozystack/etcd-operator` |
| Helm chart | none | `oci://ghcr.io/cozystack/charts/etcd-operator` |

The API group changed, so the Cozystack operator does not pick up `EtcdCluster`
objects created under `lllamnyp.su`. A spec written for this repository is still
valid there once its `apiVersion` is changed. Applying it creates a new, empty
etcd cluster, though. To carry data over, take an `etcdctl snapshot` and restore
it at bootstrap with `spec.bootstrap.restore`.

Start with the
[installation guide](https://github.com/cozystack/etcd-operator/blob/main/docs/installation.md)
in the new repository. The `docs/` directory here describes the code as it
stood at `9a6cc65` and is not maintained.

## License

Apache 2.0.
