# etcd operator

This project is in early design stages, hence even architectural decisions are likely to change and internal inconsistencies are possible and even likely.

## Motivation

This operator is designed with the primary intent of self-hosting a kubernetes' etcd cluster and optionally using the servers' RAM as a storage medium. Consequently, a necessary ability is to connect to an existing etcd cluster (i.e. the initial cluster used by the kubernetes cluster) and enlarge it by spawning new etcd peers.

Other desirable or mandatory features include:

- Self-healing, i.e. deletion of unhealthy members and the creation of new peers in their place.
- Rapid creation of etcd clusters for general purposes.
- Simplified TLS certificate and key management.
- Enforcement of secure defaults.
- A far-reaching goal may be to support persistent clusters that survive a complete outage when using persistent storage (`hostpath`). This is inapplicable for etcd clusters backing kubernetes.
- An even further goal may be, nonetheless, to provide a simplified path to recovery in the event of a complete outage of such a cluster.

Non-goals are:

- Arbitrary configurability, such as the implementation of support for the entire set of flags of the etcd binary.
- etcdV2 support.
- Management of the bootstrap cluster.
- Provisioning and management of storage beyond `hostPath` and `emptyDir`.

## Workflow

### Attaching to an existing cluster

1. The set of CAs, certificates, and private keys of the existing cluster is gathered and added to the target namespace as kubernetes secrets by the user.
2. The existing etcd peers are described as `EtcdBootstrapPeer` objects and created in the cluster.
3. An `EtcdCluster` object is created, targeting the `EtcdBootstrapPeer` objects.
4. The operator generates keys and certificates for new members of the etcd cluster.
5. A new etcd peer is spawned as an `EtcdPeer` object.
6. The operator waits for the new `EtcdPeer` to report as healthy.
7. The previous two steps are repeated as many times as necessary.

### Removing the bootstrap cluster

1. The user updates the `EtcdCluster` object to signal that the bootstrap cluster is no longer necessary.
2. The operator calls the etcd API to remove the initial members.
3. The operator updates the `EtcdCluster` object to disown and stop tracking the `EtcdBootstrapPeer` objects.
4. The user manually stops the initial etcd peers.

