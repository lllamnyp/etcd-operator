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
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// EtcdClusterClient is the subset of the etcd v3 client used by the operator.
// Defined as an interface so tests can substitute a fake without dialing a real
// etcd cluster. *clientv3.Client satisfies this via its embedded Cluster
// interface.
type EtcdClusterClient interface {
	MemberList(ctx context.Context) (*clientv3.MemberListResponse, error)
	MemberAdd(ctx context.Context, peerAddrs []string) (*clientv3.MemberAddResponse, error)
	MemberAddAsLearner(ctx context.Context, peerAddrs []string) (*clientv3.MemberAddResponse, error)
	MemberPromote(ctx context.Context, id uint64) (*clientv3.MemberPromoteResponse, error)
	MemberRemove(ctx context.Context, id uint64) (*clientv3.MemberRemoveResponse, error)
	Close() error
}

// EtcdClientFactory builds an EtcdClusterClient for a set of endpoints.
// Reconcilers hold a factory rather than a concrete client so tests can inject
// a fake. Production uses DefaultEtcdClientFactory.
type EtcdClientFactory func(ctx context.Context, endpoints []string) (EtcdClusterClient, error)

// DefaultEtcdClientFactory returns a real clientv3.Client.
func DefaultEtcdClientFactory(_ context.Context, endpoints []string) (EtcdClusterClient, error) {
	return clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
	})
}
