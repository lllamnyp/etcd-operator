module github.com/lllamnyp/etcd-operator

go 1.14

require (
	github.com/go-logr/logr v0.1.0
	github.com/onsi/ginkgo v1.11.0
	github.com/onsi/gomega v1.8.1
	// necessary imports to use etcd client library
	// https://github.com/etcd-io/etcd/issues/12569
	go.etcd.io/etcd v0.5.0-alpha.5.0.20200910180754-dd1b699fc489
	google.golang.org/genproto v0.0.0-20201210142538-e3217bee35cc // indirect
	google.golang.org/grpc v1.34.0 // indirect
	k8s.io/apimachinery v0.17.2
	k8s.io/client-go v0.17.2
	sigs.k8s.io/controller-runtime v0.5.0
)

// replacements for etcd client library
// https://github.com/etcd-io/etcd/issues/12569
replace (
	github.com/coreos/etcd => github.com/coreos/etcd v3.3.13+incompatible
	go.etcd.io/bbolt => go.etcd.io/bbolt v1.3.5
	go.etcd.io/etcd => go.etcd.io/etcd v0.5.0-alpha.5.0.20200910180754-dd1b699fc489 // ae9734ed278b is the SHA for git tag v3.4.13
	google.golang.org/grpc => google.golang.org/grpc v1.27.1
)
