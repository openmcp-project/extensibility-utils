package objectmanager

import (
	"github.com/openmcp-project/controller-utils/pkg/clusters"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ClusterType identifies the role of a target cluster.
type ClusterType string

const (
	ManagedControlPlane ClusterType = "ManagedControlPlane"
	PlatformCluster     ClusterType = "PlatformCluster"
	WorkloadCluster     ClusterType = "WorkloadCluster"
)

// Cluster holds objects that are reconciled against one Kubernetes client.
type Cluster interface {
	AddObject(Object)
	Objects() []Object
	DefaultNamespace() string
	Client() client.Client
	ClusterType() ClusterType
	Cluster() *clusters.Cluster
}

type cluster struct {
	cluster          *clusters.Cluster
	objects          []Object
	defaultNamespace string
	clusterType      ClusterType
}

var _ Cluster = (*cluster)(nil)

// NewCluster creates an object-manager cluster from a Kubernetes client.
func NewCluster(c *clusters.Cluster, namespace string, clusterType ClusterType) Cluster {
	return &cluster{
		cluster:          c,
		objects:          []Object{},
		defaultNamespace: namespace,
		clusterType:      clusterType,
	}
}

func (c *cluster) AddObject(object Object) { c.objects = append(c.objects, object) }

func (c *cluster) Objects() []Object { return c.objects }

func (c *cluster) DefaultNamespace() string { return c.defaultNamespace }

func (c *cluster) Client() client.Client { return c.cluster.Client() }

func (c *cluster) ClusterType() ClusterType { return c.clusterType }

func (c *cluster) Cluster() *clusters.Cluster { return c.cluster }
