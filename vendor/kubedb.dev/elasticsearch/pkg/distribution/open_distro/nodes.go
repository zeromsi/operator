/*
Copyright AppsCode Inc. and Contributors

Licensed under the PolyForm Noncommercial License 1.0.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://github.com/appscode/licenses/raw/1.0.0/PolyForm-Noncommercial-1.0.0.md

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package open_distro

import (
	"fmt"
	"strings"

	api "kubedb.dev/apimachinery/apis/kubedb/v1alpha1"

	"github.com/appscode/go/types"
	corev1 "k8s.io/api/core/v1"
	kutil "kmodules.xyz/client-go"
	core_util "kmodules.xyz/client-go/core/v1"
)

const (
	DefaultClientNodePrefix = "client"
	DefaultDataNodePrefix   = "data"
	DefaultMasterNodePrefix = "master"

	NodeRoleMaster = "node.role.master"
	NodeRoleClient = "node.role.client"
	NodeRoleData   = "node.role.data"
	NodeRoleSet    = "set"

	// 26GB, 26 * 1024 * 1024 * 1024 = 27917287424
	MaxHeapSize = 27917287424
)

func (es *Elasticsearch) EnsureMasterNodes() (kutil.VerbType, error) {
	statefulSetName := es.elasticsearch.OffshootName()
	masterNode := es.elasticsearch.Spec.Topology.Master

	if masterNode.Prefix != "" {
		statefulSetName = fmt.Sprintf("%v-%v", masterNode.Prefix, statefulSetName)
	} else {
		statefulSetName = fmt.Sprintf("%v-%v", DefaultMasterNodePrefix, statefulSetName)
	}

	labels := map[string]string{
		NodeRoleMaster: NodeRoleSet,
	}

	// If replicas is not provided, default to 1.
	replicas := types.Int32P(1)
	if masterNode.Replicas != nil {
		replicas = masterNode.Replicas
	}

	heapSize := int64(134217728) // 128mb
	if request, found := masterNode.Resources.Requests[corev1.ResourceMemory]; found && request.Value() > 0 {
		heapSize = getHeapSizeForNode(request.Value())
	}

	// Environment variable list for main container.
	// These are node specific, i.e. changes depending on node type.
	// Following are for Master node:
	envList := []corev1.EnvVar{
		{
			Name:  "ES_JAVA_OPTS",
			Value: fmt.Sprintf("-Xms%v -Xmx%v", heapSize, heapSize),
		},
		{
			Name:  "node.ingest",
			Value: "false",
		},
		{
			Name:  "node.master",
			Value: "true",
		},
		{
			Name:  "node.data",
			Value: "false",
		},
	}

	// These Env are only required for master nodes to bootstrap
	// for the vary first time. Need to remove from EnvList as
	// soon as the cluster is up and running.
	if strings.HasPrefix(es.esVersion.Spec.Version, "1.") {
		envList = core_util.UpsertEnvVars(envList, corev1.EnvVar{
			Name:  "cluster.initial_master_nodes",
			Value: es.getInitialMasterNodes(),
		})
	} else {
		envList = core_util.UpsertEnvVars(envList, corev1.EnvVar{
			Name:  "discovery.zen.minimum_master_nodes",
			Value: fmt.Sprintf("%v", (*replicas/2)+1),
		})
	}

	// Upsert common environment variables.
	// These are same for all type of node.
	envList = es.upsertContainerEnv(envList)

	// add/overwrite user provided env; these are provided via crd spec
	envList = core_util.UpsertEnvVars(envList, es.elasticsearch.Spec.PodTemplate.Spec.Env...)

	// Environment variables for init container (i.e. config-merger)
	initEnvList := []corev1.EnvVar{
		{
			Name:  "NODE_MASTER",
			Value: "true",
		},
		{
			Name:  "NODE_DATA",
			Value: "false",
		},
		{
			Name:  "NODE_INGEST",
			Value: "false",
		},
	}

	return es.ensureStatefulSet(&masterNode, statefulSetName, labels, replicas, NodeRoleMaster, envList, initEnvList)
}

func (es *Elasticsearch) EnsureDataNodes() (kutil.VerbType, error) {
	statefulSetName := es.elasticsearch.OffshootName()
	dataNode := es.elasticsearch.Spec.Topology.Data

	if dataNode.Prefix != "" {
		statefulSetName = fmt.Sprintf("%v-%v", dataNode.Prefix, statefulSetName)
	} else {
		statefulSetName = fmt.Sprintf("%v-%v", DefaultDataNodePrefix, statefulSetName)
	}

	labels := map[string]string{
		NodeRoleData: NodeRoleSet,
	}

	heapSize := int64(134217728) // 128mb
	if request, found := dataNode.Resources.Requests[corev1.ResourceMemory]; found && request.Value() > 0 {
		heapSize = getHeapSizeForNode(request.Value())
	}

	// Environment variable list for main container.
	// These are node specific, i.e. changes depending on node type.
	// Following are for Data node:
	envList := []corev1.EnvVar{
		{
			Name:  "ES_JAVA_OPTS",
			Value: fmt.Sprintf("-Xms%v -Xmx%v", heapSize, heapSize),
		},
		{
			Name:  "node.ingest",
			Value: "false",
		},
		{
			Name:  "node.master",
			Value: "false",
		},
		{
			Name:  "node.data",
			Value: "true",
		},
	}
	// Upsert common environment variables.
	// These are same for all type of node.
	envList = es.upsertContainerEnv(envList)

	// add/overwrite user provided env; these are provided via crd spec
	envList = core_util.UpsertEnvVars(envList, es.elasticsearch.Spec.PodTemplate.Spec.Env...)

	// Environment variables for init container (i.e. config-merger)
	initEnvList := []corev1.EnvVar{
		{
			Name:  "NODE_MASTER",
			Value: "false",
		},
		{
			Name:  "NODE_DATA",
			Value: "true",
		},
		{
			Name:  "NODE_INGEST",
			Value: "false",
		},
	}

	replicas := types.Int32P(1)
	if dataNode.Replicas != nil {
		replicas = dataNode.Replicas
	}

	return es.ensureStatefulSet(&dataNode, statefulSetName, labels, replicas, NodeRoleData, envList, initEnvList)

}

func (es *Elasticsearch) EnsureClientNodes() (kutil.VerbType, error) {
	statefulSetName := es.elasticsearch.OffshootName()
	clientNode := es.elasticsearch.Spec.Topology.Client

	if clientNode.Prefix != "" {
		statefulSetName = fmt.Sprintf("%v-%v", clientNode.Prefix, statefulSetName)
	} else {
		statefulSetName = fmt.Sprintf("%v-%v", DefaultClientNodePrefix, statefulSetName)
	}

	labels := map[string]string{
		NodeRoleClient: NodeRoleSet,
	}

	heapSize := int64(134217728) // 128mb
	if request, found := clientNode.Resources.Requests[corev1.ResourceMemory]; found && request.Value() > 0 {
		heapSize = getHeapSizeForNode(request.Value())
	}

	// Environment variable list for main container.
	// These are node specific, i.e. changes depending on node type.
	// Following are for Client node:
	envList := []corev1.EnvVar{
		{
			Name:  "ES_JAVA_OPTS",
			Value: fmt.Sprintf("-Xms%v -Xmx%v", heapSize, heapSize),
		},
		{
			Name:  "node.ingest",
			Value: "true",
		},
		{
			Name:  "node.master",
			Value: "false",
		},
		{
			Name:  "node.data",
			Value: "false",
		},
	}
	// Upsert common environment variables.
	// These are same for all type of node.
	envList = es.upsertContainerEnv(envList)

	// add/overwrite user provided env; these are provided via crd spec
	envList = core_util.UpsertEnvVars(envList, es.elasticsearch.Spec.PodTemplate.Spec.Env...)

	// Environment variables for init container (i.e. config-merger)
	initEnvList := []corev1.EnvVar{
		{
			Name:  "NODE_MASTER",
			Value: "false",
		},
		{
			Name:  "NODE_DATA",
			Value: "false",
		},
		{
			Name:  "NODE_INGEST",
			Value: "true",
		},
	}

	replicas := types.Int32P(1)
	if clientNode.Replicas != nil {
		replicas = clientNode.Replicas
	}

	return es.ensureStatefulSet(&clientNode, statefulSetName, labels, replicas, NodeRoleClient, envList, initEnvList)
}

func (es *Elasticsearch) EnsureCombinedNode() (kutil.VerbType, error) {
	statefulSetName := es.elasticsearch.OffshootName()
	combinedNode := es.getCombinedNode()

	// Each node performs all three roles; master, data, and client.
	labels := map[string]string{
		NodeRoleMaster: NodeRoleSet,
		NodeRoleData:   NodeRoleSet,
		NodeRoleClient: NodeRoleSet,
	}

	// If replicas is not provided, default to 1.
	replicas := types.Int32P(1)
	if combinedNode.Replicas != nil {
		replicas = combinedNode.Replicas
	}

	heapSize := int64(134217728) // 128mb
	if request, found := combinedNode.Resources.Requests[corev1.ResourceMemory]; found && request.Value() > 0 {
		heapSize = getHeapSizeForNode(request.Value())
	}

	// Environment variable list for main container.
	// These are node specific, i.e. changes depending on node type.
	// Followings are for Combined node:
	envList := []corev1.EnvVar{
		{
			Name:  "ES_JAVA_OPTS",
			Value: fmt.Sprintf("-Xms%v -Xmx%v", heapSize, heapSize),
		},
		{
			Name:  "node.ingest",
			Value: "true",
		},
		{
			Name:  "node.master",
			Value: "true",
		},
		{
			Name:  "node.data",
			Value: "true",
		},
	}

	// These Env are only required for master nodes to bootstrap
	// for the vary first time. Need to remove from EnvList as
	// soon as the cluster is up and running.
	if strings.HasPrefix(es.esVersion.Spec.Version, "1.") {
		envList = core_util.UpsertEnvVars(envList, corev1.EnvVar{
			Name:  "cluster.initial_master_nodes",
			Value: es.getInitialMasterNodes(),
		})
	} else {
		envList = core_util.UpsertEnvVars(envList, corev1.EnvVar{
			Name:  "discovery.zen.minimum_master_nodes",
			Value: fmt.Sprintf("%v", (*replicas/2)+1),
		})
	}

	// Upsert common environment variables.
	// These are same for all type of node.
	envList = es.upsertContainerEnv(envList)

	// add/overwrite user provided env; these are provided via crd spec
	envList = core_util.UpsertEnvVars(envList, es.elasticsearch.Spec.PodTemplate.Spec.Env...)

	// Environment variables for init container (i.e. config-merger)
	initEnvList := []corev1.EnvVar{
		{
			Name:  "NODE_MASTER",
			Value: "true",
		},
		{
			Name:  "NODE_DATA",
			Value: "true",
		},
		{
			Name:  "NODE_INGEST",
			Value: "true",
		},
	}

	// For affinity, NodeRoleClient is used.
	return es.ensureStatefulSet(combinedNode, statefulSetName, labels, replicas, NodeRoleClient, envList, initEnvList)

}

// Use ElasticsearchNode struct for combined nodes too,
// to maintain the similar code structure.
func (es *Elasticsearch) getCombinedNode() *api.ElasticsearchNode {
	return &api.ElasticsearchNode{
		Replicas:       es.elasticsearch.Spec.Replicas,
		Storage:        es.elasticsearch.Spec.Storage,
		Resources:      es.elasticsearch.Spec.PodTemplate.Spec.Resources,
		MaxUnavailable: es.elasticsearch.Spec.MaxUnavailable,
	}
}

// Ref:
//	- https://www.elastic.co/guide/en/elasticsearch/reference/7.6/heap-size.html#heap-size
//  - no more than 50% of your physical RAM
//  - no more than 32GB that the JVM uses for compressed object pointers (compressed oops)
// 	- no more than 26GB for zero-based compressed oops;
func getHeapSizeForNode(val int64) int64 {
	// no more than 50% of main memory (RAM)
	ret := (val / 100) * 50

	// 26 GB is safe on most systems
	if ret > MaxHeapSize {
		ret = MaxHeapSize
	}
	return ret
}
