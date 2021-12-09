package e2e

import (
	"context"
	api "github.com/k8ssandra/k8ssandra-operator/apis/k8ssandra/v1alpha1"
	"github.com/k8ssandra/k8ssandra-operator/test/framework"
	"github.com/k8ssandra/k8ssandra-operator/test/kubectl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"testing"
)

func multiDcMultiCluster(t *testing.T, ctx context.Context, klusterNamespace string, f *framework.E2eFramework) {
	require := require.New(t)
	assert := assert.New(t)

	dc1Namespace := "test-1"
	dc2Namespace := "test-2"

	t.Log("check that the K8ssandraCluster was created")
	k8ssandra := &api.K8ssandraCluster{}
	err := f.Client.Get(ctx, types.NamespacedName{Namespace: klusterNamespace, Name: "test"}, k8ssandra)
	require.NoError(err, "failed to get K8ssandraCluster in operatorNamespace %s", klusterNamespace)

	dc1Key := framework.ClusterKey{K8sContext: "kind-k8ssandra-0", NamespacedName: types.NamespacedName{Namespace: dc1Namespace, Name: "dc1"}}
	checkDatacenterReady(t, ctx, dc1Key, f)

	t.Log("check k8ssandra cluster status")
	require.Eventually(func() bool {
		k8ssandra := &api.K8ssandraCluster{}
		err := f.Client.Get(ctx, types.NamespacedName{Namespace: klusterNamespace, Name: "test"}, k8ssandra)
		if err != nil {
			return false
		}

		cassandraStatus := getCassandraDatacenterStatus(k8ssandra, dc1Key.Name)
		if cassandraStatus == nil {
			return false
		}
		return cassandraDatacenterReady(cassandraStatus)
	}, polling.k8ssandraClusterStatus.timeout, polling.k8ssandraClusterStatus.interval, "timed out waiting for K8ssandraCluster status to get updated")

	dc2Key := framework.ClusterKey{K8sContext: "kind-k8ssandra-1", NamespacedName: types.NamespacedName{Namespace: dc2Namespace, Name: "dc2"}}
	checkDatacenterReady(t, ctx, dc2Key, f)

	t.Log("check k8ssandra cluster status")
	require.Eventually(func() bool {
		k8ssandra := &api.K8ssandraCluster{}
		err := f.Client.Get(ctx, types.NamespacedName{Namespace: klusterNamespace, Name: "test"}, k8ssandra)
		if err != nil {
			return false
		}

		cassandraStatus := getCassandraDatacenterStatus(k8ssandra, dc1Key.Name)
		if cassandraStatus == nil {
			return false
		}
		if !cassandraDatacenterReady(cassandraStatus) {
			return false
		}

		cassandraStatus = getCassandraDatacenterStatus(k8ssandra, dc2Key.Name)
		if cassandraStatus == nil {
			return false
		}
		return cassandraDatacenterReady(cassandraStatus)
	}, polling.k8ssandraClusterStatus.timeout, polling.k8ssandraClusterStatus.interval, "timed out waiting for K8ssandraCluster status to get updated")

	t.Log("check that nodes in dc1 see nodes in dc2")
	opts := kubectl.Options{Namespace: dc1Namespace, Context: "kind-k8ssandra-0"}
	pod := "test-dc1-rack1-sts-0"
	count := 6
	err = f.WaitForNodeToolStatusUN(opts, pod, count, polling.nodetoolStatus.timeout, polling.nodetoolStatus.interval)

	assert.NoError(err, "timed out waiting for nodetool status check against "+pod)

	t.Log("check nodes in dc2 see nodes in dc1")
	opts = kubectl.Options{Namespace: dc2Namespace, Context: "kind-k8ssandra-1"}
	pod = "test-dc2-rack1-sts-0"
	err = f.WaitForNodeToolStatusUN(opts, pod, count, polling.nodetoolStatus.timeout, polling.nodetoolStatus.interval)

	assert.NoError(err, "timed out waiting for nodetool status check against "+pod)
}