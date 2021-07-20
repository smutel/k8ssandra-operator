package framework

import (
	"context"
	"fmt"
	cassdcapi "github.com/k8ssandra/cass-operator/operator/pkg/apis/cassandra/v1beta1"
	"github.com/k8ssandra/k8ssandra-operator/test/kubectl"
	"github.com/k8ssandra/k8ssandra-operator/test/kustomize"
	"io"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"text/template"
	"time"
)

const (
	defaultControlPlaneContext = "kind-k8ssandra-0"
)

type E2eFramework struct {
	*Framework
}

func NewE2eFramework() (*E2eFramework, error) {
	configFile, err := filepath.Abs("../../build/kubeconfig")
	if err != nil {
		return nil, err
	}

	if _, err = os.Stat(configFile); err != nil {
		return nil, err
	}

	config, err := clientcmd.LoadFromFile(configFile)
	if err != nil {
		return nil, err
	}

	controlPlaneContext := ""
	var controlPlaneClient client.Client
	remoteClients := make(map[string]client.Client, 0)

	for name, _ := range config.Contexts {
		clientCfg := clientcmd.NewNonInteractiveClientConfig(*config, name, &clientcmd.ConfigOverrides{}, nil)
		restCfg, err := clientCfg.ClientConfig()

		if err != nil {
			return nil, err
		}

		remoteClient, err := client.New(restCfg, client.Options{Scheme: scheme.Scheme})
		if err != nil {
			return nil, err
		}

		// TODO Add a flag or option to allow the user to specify the control plane cluster
		//if len(controlPlaneContext) == 0 {
		//	controlPlaneContext = name
		//	controlPlaneClient = remoteClient
		//}
		remoteClients[name] = remoteClient
	}

	if remoteClient, found := remoteClients[defaultControlPlaneContext]; found {
		controlPlaneContext = defaultControlPlaneContext
		controlPlaneClient = remoteClient
	} else {
		for k8sContext, remoteClient := range remoteClients {
			controlPlaneContext = k8sContext
			controlPlaneClient = remoteClient
			break
		}
	}

	f := NewFramework(controlPlaneClient, controlPlaneContext, remoteClients)

	return &E2eFramework{Framework: f}, nil
}

func (f *E2eFramework) getRemoteClusterContexts() []string {
	contexts := make([]string, 0, len(f.remoteClients))
	for ctx, _ := range f.remoteClients {
		contexts = append(contexts, ctx)
	}
	return contexts
}

type Kustomization struct {
	Namespace string
}

func generateCassOperatorKustomization(namespace string) error {
	tmpl := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
- ../../../config/cass-operator
namespace: {{ .Namespace }}
`
	k := Kustomization{Namespace: namespace}

	return generateKustomizationFile("cass-operator", k, tmpl)
}

func generateContextsKustomization(namespace string) error {
	tmpl := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

generatorOptions:
  disableNameSuffixHash: true

secretGenerator:
- files:
  - kubeconfig
  name: k8s-contexts
namespace: {{ .Namespace }}
`
	k := Kustomization{Namespace: namespace}

	if err := generateKustomizationFile("k8s-contexts", k, tmpl); err != nil {
		return err
	}

	src := filepath.Join("..", "..", "build", "in_cluster_kubeconfig")
	dest := filepath.Join("..", "..", "build", "test-config", "k8s-contexts", "kubeconfig")

	srcFile, err := os.Create(src)
	if err != nil {
		return err
	}

	destFile, err := os.Create(dest)
	if err != nil {
		return err
	}

	_, err = io.Copy(destFile, srcFile)
	return err
}

func generateK8ssandraOperatorKustomization(namespace string) error {
	tmpl := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
- ../../../config/default
namespace: {{ .Namespace }}
`
	k := Kustomization{Namespace: namespace}

	return generateKustomizationFile("k8ssandra-operator", k, tmpl)
}

func generateKustomizationFile(name string, k Kustomization, tmpl string) error {
	dir := filepath.Join("..", "..", "build", "test-config", name)

	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	parsed, err := template.New(name).Parse(tmpl)
	if err != nil {
		return nil
	}

	file, err := os.Create(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		return err
	}

	return parsed.Execute(file, k)
}


func (f *E2eFramework) kustomizeAndApply(dir, namespace string, contexts ...string) error {
	kdir, err := filepath.Abs(dir)
	if err != nil {
		f.logger.Error(err, "failed to get full path", "dir", dir)
		return err
	}

	if err := kustomize.SetNamespace(kdir, namespace); err != nil {
		f.logger.Error(err, "failed to set namespace for kustomization directory", "dir", kdir)
		return err
	}

	if len(contexts) == 0 {
		buf, err := kustomize.Build(kdir)
		if err != nil {
			f.logger.Error(err, "kustomize build failed", "dir", kdir)
			return err
		}

		options := kubectl.Options{Namespace: namespace}
		return kubectl.Apply(options, buf)
	}

	for _, ctx := range contexts {
		buf, err := kustomize.Build(kdir)
		if err != nil {
			f.logger.Error(err, "kustomize build failed", "dir", kdir)
			return err
		}

		options := kubectl.Options{Namespace: namespace, Context: ctx}
		if err := kubectl.Apply(options, buf); err != nil {
			return err
		}
	}

	return nil
}

// DeployK8ssandraOperator Deploys k8ssandra-operator in the control plane cluster. Note
// that the control plane cluster can also be one of the remote clusters.
func (f *E2eFramework) DeployK8ssandraOperator(namespace string) error {
	if err := generateK8ssandraOperatorKustomization(namespace); err != nil {
		return err
	}

	dir := filepath.Join("..", "..", "build", "test-config", "k8ssandra-operator")

	return f.kustomizeAndApply(dir, namespace);
}

// DeployCassOperator deploys cass-operator in all remote clusters.
func (f *E2eFramework) DeployCassOperator(namespace string) error {
	if err := generateCassOperatorKustomization(namespace); err != nil {
		return err
	}

	dir := filepath.Join("..", "..", "build", "test-config", "cass-operator")

	return f.kustomizeAndApply(dir, namespace, f.getRemoteClusterContexts()...)
}

// DeployK8sContextsSecret Deploys the contexts secret in the control plane cluster.
func (f *E2eFramework) DeployK8sContextsSecret(namespace string) error {
	if err := generateContextsKustomization(namespace); err != nil {
		return err
	}

	dir := filepath.Join("..", "..", "build", "test-config", "cass-operator")

	return f.kustomizeAndApply(dir, namespace, f.controlPlaneContext)
}

// DeleteNamespace Deletes the namespace from all remote clusters and blocks until they
// have completely terminated.
func (f *E2eFramework) DeleteNamespace(name string, timeout, interval time.Duration) error {
	// TODO Make sure we delete from the control plane cluster as well

	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	for k8sContext, remoteClient := range f.remoteClients {
		f.logger.WithValues("deleting namespace", "Namespace", name, "Context", k8sContext)
		if err := remoteClient.Delete(context.Background(), namespace.DeepCopy()); err != nil {
			return err
		}
	}

	// Should this wait.Poll call be per cluster?
	return wait.Poll(interval, timeout, func() (bool, error) {
		for _, remoteClient := range f.remoteClients {
			err := remoteClient.Get(context.TODO(), types.NamespacedName{Name: name}, namespace.DeepCopy())

			if err == nil || !apierrors.IsNotFound(err) {
				return false, nil
			}
		}

		return true, nil
	})
}

func (f *E2eFramework) WaitForCrdsToBecomeActive() error {
	// TODO Add multi-cluster support.
	// By default this should wait for all clusters including the control plane cluster.

	return kubectl.WaitForCondition("established", "--timeout=60s", "--all", "crd")
}

// WaitForK8ssandraOperatorToBeReady blocks until the k8ssandra-operator deployment is
// ready in the control plane cluster.
func (f *E2eFramework) WaitForK8ssandraOperatorToBeReady(namespace string, timeout, interval time.Duration) error {
	key := ClusterKey{
		K8sContext: f.controlPlaneContext,
		NamespacedName: types.NamespacedName{Namespace: namespace, Name: "k8ssandra-operator"},
	}
	return f.WaitForDeploymentToBeReady(key, timeout, interval)
}

// WaitForCassOperatorToBeReady blocks until the cass-operator deployment is ready in all
// clusters.
func (f *E2eFramework) WaitForCassOperatorToBeReady(namespace string, timeout, interval time.Duration) error {
	key := ClusterKey{NamespacedName: types.NamespacedName{Namespace: namespace, Name: "cass-operator"}}
	return f.WaitForDeploymentToBeReady(key, timeout, interval)
}

// DumpClusterInfo Executes `kubectl cluster-info dump -o yaml` on each cluster. The output
// is stored under <project-root>/build/test.
func (f *E2eFramework) DumpClusterInfo(test, namespace string) error {
	f.logger.Info("dumping cluster info")

	now := time.Now()
	baseDir := fmt.Sprintf("../../build/test/%s/%d-%d-%d-%d-%d", test, now.Year(), now.Month(), now.Day(), now.Hour(), now.Second())
	errs := make([]error, 0)

	for ctx, _ := range f.remoteClients {
		outputDir := filepath.Join(baseDir, ctx)
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			errs = append(errs, fmt.Errorf("failed to make output for cluster %s: %w", ctx, err))
			return err
		}

		opts := kubectl.ClusterInfoOptions{Options: kubectl.Options{Namespace: namespace, Context: ctx}, OutputDirectory: outputDir}
		if err := kubectl.DumpClusterInfo(opts); err != nil {
			errs = append(errs, fmt.Errorf("failed to dump cluster info for cluster %s: %w", ctx, err))
		}
	}

	if len(errs) > 0 {
		return errors.NewAggregate(errs)
	}
	return nil
}

// DeleteDatacenters deletes all CassandraDatacenters in namespace in all remote clusters.
// This function blocks until all pods from all CassandraDatacenters have terminated.
func (f *E2eFramework) DeleteDatacenters(namespace string, timeout, interval time.Duration) error {
	f.logger.Info("deleting all CassandraDatacenters", "Namespace", namespace)

	for _, remoteClient := range f.remoteClients {
		ctx := context.TODO()
		dc := &cassdcapi.CassandraDatacenter{}

		if err := remoteClient.DeleteAllOf(ctx, dc, client.InNamespace(namespace)); err != nil {
			return err
		}
	}

	// Should there be a separate wait.Poll call per cluster?
	return wait.Poll(interval, timeout, func() (bool, error) {
		for k8sContext, remoteClient := range f.remoteClients {
			list := &corev1.PodList{}
			if err := remoteClient.List(context.TODO(), list, client.InNamespace(namespace), client.HasLabels{cassdcapi.ClusterLabel}); err != nil {
				f.logger.Error(err, "failed to list datacenter pods", "Context", k8sContext)
				return false, err
			}

			if len(list.Items) > 0 {
				return false, nil
			}
		}

		return true, nil
	})
}

func (f *E2eFramework) UndeployK8ssandraOperator(namespace string) error {
	dir, err := filepath.Abs("../testdata/k8ssandra-operator")
	if err != nil {
		return err
	}

	buf, err := kustomize.Build(dir)
	if err != nil {
		return err
	}

	options := kubectl.Options{Namespace: namespace}

	return kubectl.Delete(options, buf)
}
