/*
Copyright 2020 Timofey Larkin.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	_ "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	etcdv1alpha1 "github.com/lllamnyp/etcd-operator/api/v1alpha1"

	_ "github.com/lllamnyp/etcd-operator/internal/etcd"
)

// EtcdClusterReconciler reconciles a EtcdCluster object
type EtcdClusterReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=etcd.lllamnyp.su,resources=etcdclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=etcd.lllamnyp.su,resources=etcdclusters/status,verbs=get;update;patch

func (r *EtcdClusterReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("etcdcluster", req.NamespacedName)

	// your logic here
	var c etcdv1alpha1.EtcdCluster

	if err := r.Get(ctx, req.NamespacedName, &c); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var pL etcdv1alpha1.EtcdPeerList
	var matchingLabels client.MatchingLabels = c.Spec.Selector
	if err := r.List(ctx, &pL, client.InNamespace(req.Namespace), matchingLabels); err != nil {
		log.Error(err, "Couldn't get controlled peers")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	for _ = range pL.Items {
	}

	if c.Status.Phase == etcdv1alpha1.PhaseNew {
		log.Info("New cluster")
	}

	if c.PeerCount() < c.Spec.ClusterSize {
		p := c.CreatePeer()
		r.Create(ctx, p)
	}

	return ctrl.Result{RequeueAfter: time.Second * 5}, nil
}

func (r *EtcdClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&etcdv1alpha1.EtcdCluster{}).
		Complete(r)
}
