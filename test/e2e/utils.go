package e2e

import (
	"bytes"
	goctx "context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	aerospikev1alpha1 "github.com/aerospike/aerospike-kubernetes-operator/pkg/apis/aerospike/v1alpha1"
	operatorutils "github.com/aerospike/aerospike-kubernetes-operator/pkg/controller/utils"

	"github.com/operator-framework/operator-sdk/pkg/test"
	framework "github.com/operator-framework/operator-sdk/pkg/test"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

var (
	cleanupRetryInterval = time.Second * 1
	cleanupTimeout       = time.Second * 200
)

var schemas map[string]string
var secrets map[string][]byte

const schemaDir = "deploy/config-schemas"
const secretDir = "deploy/secrets"

const tlsSecretName = "aerospike-secret"
const authSecretName = "auth"
const authSecretNameForUpdate = "auth-update"

var aerospikeVolumeInitMethodDeleteFiles = aerospikev1alpha1.AerospikeVolumeInitMethodDeleteFiles

func cleanupOption(ctx *framework.TestCtx) *framework.CleanupOptions {
	return &framework.CleanupOptions{TestContext: ctx, Timeout: cleanupTimeout, RetryInterval: cleanupRetryInterval}
}

func initConf(schemaDir string) error {
	schemas = make(map[string]string)

	fileInfo, err := ioutil.ReadDir(schemaDir)
	if err != nil {
		return err
	}

	if len(fileInfo) == 0 {
		return fmt.Errorf("no config schema file available in %s", schemaDir)
	}

	for _, file := range fileInfo {
		if file.IsDir() {
			// no need to check recursively
			continue
		}

		schema, err := ioutil.ReadFile(filepath.Join(schemaDir, file.Name()))
		if err != nil {
			return fmt.Errorf("wrong config schema file %s: %v", file.Name(), err)
		}

		schemas[file.Name()] = string(schema)
	}

	return nil
}

func initConfigSecret(secretDir string) error {
	secrets = make(map[string][]byte)

	fileInfo, err := ioutil.ReadDir(secretDir)
	if err != nil {
		return err
	}

	if len(fileInfo) == 0 {
		return fmt.Errorf("no secret file available in %s", secretDir)
	}

	for _, file := range fileInfo {
		if file.IsDir() {
			// no need to check recursively
			continue
		}

		secret, err := ioutil.ReadFile(filepath.Join(secretDir, file.Name()))
		if err != nil {
			return fmt.Errorf("wrong secret file %s: %v", file.Name(), err)
		}

		secrets[file.Name()] = secret
	}

	return nil
}

func setupByUser(f *framework.Framework, ctx *framework.TestCtx) error {
	// kubectl create configmap config-schemas --from-file=deploy/config-schemas
	namespace, err := ctx.GetNamespace()
	if err != nil {
		return fmt.Errorf("Could not get namespace: %v", err)
	}

	labels := getLabels()

	// Create configSecret
	if err := initConfigSecret(secretDir); err != nil {
		return fmt.Errorf("Failed to init secrets: %v", err)
	}

	if err := createConfigSecret(f, ctx, namespace, labels); err != nil {
		return err
	}

	// Create authSecret
	pass := "admin"
	if err := createAuthSecret(f, ctx, namespace, labels, authSecretName, pass); err != nil {
		return err
	}

	// Create another authSecret. Used in access-control tests
	passUpdate := "admin321"
	if err := createAuthSecret(f, ctx, namespace, labels, authSecretNameForUpdate, passUpdate); err != nil {
		return err
	}

	// Create preReq for multiclusters
	if err := createClusterResource(f, ctx); err != nil {
		return err
	}
	if err := createClusterPreReq(f, ctx, multiClusterNs1); err != nil {
		return err
	}
	if err := createClusterPreReq(f, ctx, multiClusterNs2); err != nil {
		return err
	}

	return nil
}

func createClusterPreReq(f *framework.Framework, ctx *framework.TestCtx, namespace string) error {
	labels := getLabels()

	if err := createConfigSecret(f, ctx, namespace, labels); err != nil {
		return err
	}

	// Create authSecret
	pass := "admin"
	if err := createAuthSecret(f, ctx, namespace, labels, authSecretName, pass); err != nil {
		return err
	}

	return nil
}

func createConfigSecret(f *framework.Framework, ctx *framework.TestCtx, namespace string, labels map[string]string) error {
	// Create configSecret
	s := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tlsSecretName,
			Namespace: namespace,
			Labels:    labels,
		},
		Type: v1.SecretTypeOpaque,
		Data: secrets,
	}
	// use TestCtx's create helper to create the object and add a cleanup function for the new object
	err := f.Client.Create(goctx.TODO(), s, cleanupOption(ctx))
	if err != nil {
		return err
	}
	return nil
}

func createAuthSecret(f *framework.Framework, ctx *framework.TestCtx, namespace string, labels map[string]string, secretName, pass string) error {

	// Create authSecret
	as := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
			Labels:    labels,
		},
		Type: v1.SecretTypeOpaque,
		Data: map[string][]byte{
			"password": []byte(pass),
		},
	}
	// use TestCtx's create helper to create the object and add a cleanup function for the new object
	err := f.Client.Create(goctx.TODO(), as, cleanupOption(ctx))
	if err != nil {
		return err
	}

	return nil
}

func getLabels() map[string]string {
	return map[string]string{"app": "aerospike-cluster"}
}

// WaitForOperatorDeployment has the same functionality as WaitForDeployment but will no wait for the deployment if the
// test was run with a locally run operator (--up-local flag)
func waitForOperatorDeployment(t *testing.T, kubeclient kubernetes.Interface, namespace, name string, replicas int, retryInterval, timeout time.Duration) error {
	return waitForDeployment(t, kubeclient, namespace, name, replicas, retryInterval, timeout, true)
}

func waitForDeployment(t *testing.T, kubeclient kubernetes.Interface, namespace, name string, replicas int, retryInterval, timeout time.Duration, isOperator bool) error {
	if isOperator && test.Global.LocalOperator {
		t.Log("Operator is running locally; skip waitForDeployment")
		return nil
	}
	err := wait.Poll(retryInterval, timeout, func() (done bool, err error) {
		deployment, err := kubeclient.AppsV1().Deployments(namespace).Get(name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				t.Logf("Waiting for availability of %s deployment\n", name)
				return false, nil
			}
			return false, err
		}

		if int(deployment.Status.AvailableReplicas) == replicas {
			return true, nil
		}
		t.Logf("Waiting for full availability of %s deployment (%d/%d)\n", name, deployment.Status.AvailableReplicas, replicas)
		return false, nil
	})
	if err != nil {
		return err
	}
	t.Logf("Deployment available (%d/%d)\n", replicas, replicas)
	return nil
}

func waitForAerospikeCluster(t *testing.T, f *framework.Framework, aeroCluster *aerospikev1alpha1.AerospikeCluster, replicas int, retryInterval, timeout time.Duration) error {
	var isValid bool
	err := wait.Poll(retryInterval, timeout, func() (done bool, err error) {
		// Fetch the AerospikeCluster instance
		newCluster := &aerospikev1alpha1.AerospikeCluster{}
		err = f.Client.Get(goctx.TODO(), types.NamespacedName{Name: aeroCluster.Name, Namespace: aeroCluster.Namespace}, newCluster)
		if err != nil {
			if apierrors.IsNotFound(err) {
				t.Logf("Waiting for availability of %s AerospikeCluster\n", aeroCluster.Name)
				return false, nil
			}
			return false, err
		}
		// t.Logf("Waiting for full availability of %s AerospikeCluster (%d/%d)\n", aeroCluster.Name, aeroCluster.Status.Size, replicas)

		isValid = isClusterStateValid(t, f, aeroCluster, newCluster, replicas)
		return isValid, nil
		// return isClusterStateValid(t, f, aeroCluster, newCluster, replicas), nil
	})
	if err != nil {
		return err
	}
	if !isValid {
		return fmt.Errorf("Cluster state not matching with desired state")
	}
	t.Logf("AerospikeCluster available\n")

	// make info call
	return nil
}

func isClusterStateValid(t *testing.T, f *framework.Framework, aeroCluster *aerospikev1alpha1.AerospikeCluster, newCluster *aerospikev1alpha1.AerospikeCluster, replicas int) bool {
	if int(newCluster.Status.Size) != replicas {
		t.Logf("Cluster size is not correct")
		return false
	}
	if !reflect.DeepEqual(newCluster.Status.AerospikeClusterSpec, newCluster.Spec) {
		t.Logf("Cluster status not updated")
		return false
	}
	if len(newCluster.Status.Pods) != replicas {
		t.Logf("Cluster status doesn't have pod status for all nodes. Cluster status may not have fully updated")
		return false
	}

	for _, pod := range newCluster.Status.Pods {
		if pod.Aerospike.NodeID == "" {
			t.Logf("Cluster pod's nodeID is empty")
			return false
		}
		if operatorutils.IsImageEqual(pod.Image, aeroCluster.Spec.Image) {
			break
		}

		t.Logf("Cluster pod's image %s not same as spec %s", pod.Image, aeroCluster.Spec.Image)
	}
	return true
}

func getTimeout(nodes int32) time.Duration {
	return (5 * time.Minute * time.Duration(nodes))
}

func validateError(t *testing.T, err error, msg string) {
	if err == nil {
		t.Fatal(msg)
	} else {
		t.Log(err)
	}
}

// ExecuteCommandOnPod executes a command in the specified container,
// returning stdout, stderr and error.
func ExecuteCommandOnPod(pod *v1.Pod, containerName string, cmd ...string) (string, string, error) {
	f := framework.Global
	req := f.KubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.GetNamespace()).
		SubResource("exec").
		Param("container", containerName)
	req.VersionedParams(&v1.PodExecOptions{
		Container: containerName,
		Command:   cmd,
		Stdin:     false,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}, scheme.ParameterCodec)

	var stdout, stderr bytes.Buffer

	exec, err := remotecommand.NewSPDYExecutor(f.KubeConfig, "POST", req.URL())
	if err != nil {
		return "", "", err
	}
	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  nil,
		Stdout: &stdout,
		Stderr: &stderr,
	})

	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
}

func getRackID(pod *v1.Pod) (int, error) {
	rack, ok := pod.ObjectMeta.Labels["aerospike.com/rack-id"]
	if !ok {
		return 0, nil
	}

	return strconv.Atoi(rack)
}

// Make a deep copy from src into dst.
func Copy(dst interface{}, src interface{}) error {
	if dst == nil {
		return fmt.Errorf("dst cannot be nil")
	}
	if src == nil {
		return fmt.Errorf("src cannot be nil")
	}
	bytes, err := json.Marshal(src)
	if err != nil {
		return fmt.Errorf("Unable to marshal src: %s", err)
	}
	err = json.Unmarshal(bytes, dst)
	if err != nil {
		return fmt.Errorf("Unable to unmarshal into dst: %s", err)
	}
	return nil
}
