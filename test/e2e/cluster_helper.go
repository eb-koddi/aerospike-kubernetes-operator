package e2e

import (
	"context"
	goctx "context"
	"fmt"
	"testing"
	"time"

	aerospikev1alpha1 "github.com/aerospike/aerospike-kubernetes-operator/pkg/apis/aerospike/v1alpha1"
	"github.com/aerospike/aerospike-kubernetes-operator/pkg/controller/utils"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	lib "github.com/aerospike/aerospike-management-lib"
)

const (
	latestClusterImage = "aerospike/aerospike-server-enterprise:5.4.0.5"
	imageToUpgrade     = "aerospike/aerospike-server-enterprise:5.5.0.3"
)

var (
	retryInterval = time.Second * 5
	timeout       = time.Second * 120

	cascadeDeleteFalse = false
	cascadeDeleteTrue  = true
)

func getCluster(t *testing.T, f *framework.Framework, ctx *framework.TestCtx, clusterNamespacedName types.NamespacedName) *aerospikev1alpha1.AerospikeCluster {
	aeroCluster := &aerospikev1alpha1.AerospikeCluster{}
	err := f.Client.Get(goctx.TODO(), clusterNamespacedName, aeroCluster)
	if err != nil {
		t.Fatal(err)
	}
	return aeroCluster
}

func getClusterIfExists(t *testing.T, f *framework.Framework, ctx *framework.TestCtx, clusterNamespacedName types.NamespacedName) *aerospikev1alpha1.AerospikeCluster {
	aeroCluster := &aerospikev1alpha1.AerospikeCluster{}
	err := f.Client.Get(goctx.TODO(), clusterNamespacedName, aeroCluster)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}

		t.Fatalf("Error getting cluster: %v", err)
	}

	return aeroCluster
}

func deleteCluster(t *testing.T, f *framework.Framework, ctx *framework.TestCtx, aeroCluster *aerospikev1alpha1.AerospikeCluster) error {
	if err := f.Client.Delete(goctx.TODO(), aeroCluster); err != nil {
		return err
	}

	// Wait for all pod to get deleted.
	// TODO: Maybe add these checks in cluster delete itself.
	//time.Sleep(time.Second * 12)

	clusterNamespacedName := getClusterNamespacedName(aeroCluster.Name, aeroCluster.Namespace)
	for {
		t.Logf("Waiting for cluster %v to be deleted", aeroCluster.Name)
		existing := getClusterIfExists(t, f, ctx, clusterNamespacedName)
		if existing == nil {
			break
		}
		time.Sleep(time.Second)
	}

	// Wait for all rmoved PVCs to be terminated.
	for {
		newPVCList, err := getAeroClusterPVCList(aeroCluster, &f.Client.Client)

		if err != nil {
			t.Fatalf("Error getting PVCs: %v", err)
		}

		pending := false
		for _, pvc := range newPVCList {
			if utils.IsPVCTerminating(&pvc) {
				t.Logf("Waiting for PVC %v to terminate", pvc.Name)
				pending = true
				break
			}
		}

		if !pending {
			break
		}

		time.Sleep(time.Second)
	}

	return nil
}

func deployCluster(t *testing.T, f *framework.Framework, ctx *framework.TestCtx, aeroCluster *aerospikev1alpha1.AerospikeCluster) error {
	return deployClusterWithTO(t, f, ctx, aeroCluster, retryInterval, getTimeout(aeroCluster.Spec.Size))
}

func deployClusterWithTO(t *testing.T, f *framework.Framework, ctx *framework.TestCtx, aeroCluster *aerospikev1alpha1.AerospikeCluster, retryInterval, timeout time.Duration) error {
	// Use TestCtx's create helper to create the object and add a cleanup function for the new object
	err := f.Client.Create(goctx.TODO(), aeroCluster, cleanupOption(ctx))
	if err != nil {
		return err
	}
	// Wait for aerocluster to reach desired cluster size.
	return waitForAerospikeCluster(t, f, aeroCluster, int(aeroCluster.Spec.Size), retryInterval, timeout)
}

func updateCluster(t *testing.T, f *framework.Framework, ctx *framework.TestCtx, aeroCluster *aerospikev1alpha1.AerospikeCluster) error {
	err := f.Client.Update(goctx.TODO(), aeroCluster)
	if err != nil {
		return err
	}

	return waitForAerospikeCluster(t, f, aeroCluster, int(aeroCluster.Spec.Size), retryInterval, getTimeout(aeroCluster.Spec.Size))
}

// TODO: remove it
func updateAndWait(t *testing.T, f *framework.Framework, ctx *framework.TestCtx, aeroCluster *aerospikev1alpha1.AerospikeCluster) error {
	err := f.Client.Update(goctx.TODO(), aeroCluster)
	if err != nil {
		return err
	}
	// Currently waitForAerospikeCluster doesn't check for config update
	// How to validate if its old cluster or new cluster with new config
	// Hence sleep.
	// TODO: find another way or validate config also
	time.Sleep(5 * time.Second)

	return waitForAerospikeCluster(t, f, aeroCluster, int(aeroCluster.Spec.Size), retryInterval, getTimeout(aeroCluster.Spec.Size))
}

func getClusterPodList(f *framework.Framework, aeroCluster *aerospikev1alpha1.AerospikeCluster) (*corev1.PodList, error) {
	// List the pods for this aeroCluster's statefulset
	podList := &corev1.PodList{}
	labelSelector := labels.SelectorFromSet(utils.LabelsForAerospikeCluster(aeroCluster.Name))
	listOps := &client.ListOptions{Namespace: aeroCluster.Namespace, LabelSelector: labelSelector}

	// TODO: Should we add check to get only non-terminating pod? What if it is rolling restart
	if err := f.Client.List(context.TODO(), podList, listOps); err != nil {
		return nil, err
	}
	return podList, nil
}

func validateResource(t *testing.T, f *framework.Framework, ctx *framework.TestCtx, aeroCluster *aerospikev1alpha1.AerospikeCluster) {
	podList, err := getClusterPodList(f, aeroCluster)
	if err != nil {
		t.Fatal(err)
	}

	mem := aeroCluster.Spec.Resources.Requests.Memory()
	cpu := aeroCluster.Spec.Resources.Requests.Cpu()

	for _, p := range podList.Items {
		for _, cnt := range p.Spec.Containers {
			stMem := cnt.Resources.Requests.Memory()
			if !mem.Equal(*stMem) {
				t.Fatal(fmt.Errorf("resource memory not matching. want %v, got %v", mem.String(), stMem.String()))
			}
			limitMem := cnt.Resources.Limits.Memory()
			if !mem.Equal(*limitMem) {
				t.Fatal(fmt.Errorf("limit memory not matching. want %v, got %v", mem.String(), limitMem.String()))
			}

			stCPU := cnt.Resources.Requests.Cpu()
			if !cpu.Equal(*stCPU) {
				t.Fatal(fmt.Errorf("resource cpu not matching. want %v, got %v", cpu.String(), stCPU.String()))
			}
			limitCPU := cnt.Resources.Limits.Cpu()
			if !cpu.Equal(*limitCPU) {
				t.Fatal(fmt.Errorf("resource cpu not matching. want %v, got %v", cpu.String(), limitCPU.String()))
			}
		}
	}
}

// CreateBasicCluster deploy a basic dummy cluster with 2 nodes
func CreateBasicCluster(t *testing.T, f *framework.Framework, ctx *framework.TestCtx) {
	// get namespace
	namespace, err := ctx.GetNamespace()
	if err != nil {
		t.Fatal(err)
	}
	clusterName := "aerocluster"
	clusterNamespacedName := getClusterNamespacedName(clusterName, namespace)

	aeroCluster := createDummyAerospikeCluster(clusterNamespacedName, 2)
	t.Run("Positive", func(t *testing.T) {
		if err := deployCluster(t, f, ctx, aeroCluster); err != nil {
			t.Fatal(err)
		}
	})
}

// feature-key file needed
func createAerospikeClusterPost460(clusterNamespacedName types.NamespacedName, size int32, image string) *aerospikev1alpha1.AerospikeCluster {
	// create Aerospike custom resource
	mem := resource.MustParse("2Gi")
	cpu := resource.MustParse("200m")
	aeroCluster := &aerospikev1alpha1.AerospikeCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterNamespacedName.Name,
			Namespace: clusterNamespacedName.Namespace,
		},
		Spec: aerospikev1alpha1.AerospikeClusterSpec{
			Size:  size,
			Image: image,
			Storage: aerospikev1alpha1.AerospikeStorageSpec{
				BlockVolumePolicy: aerospikev1alpha1.AerospikePersistentVolumePolicySpec{
					InputCascadeDelete: &cascadeDeleteTrue,
				},
				FileSystemVolumePolicy: aerospikev1alpha1.AerospikePersistentVolumePolicySpec{
					InputInitMethod:    &aerospikeVolumeInitMethodDeleteFiles,
					InputCascadeDelete: &cascadeDeleteTrue,
				},
				Volumes: []aerospikev1alpha1.AerospikePersistentVolumeSpec{
					{
						Path:         "/test/dev/xvdf",
						SizeInGB:     1,
						StorageClass: "ssd",
						VolumeMode:   aerospikev1alpha1.AerospikeVolumeModeBlock,
					},
					{
						Path:         "/opt/aerospike",
						SizeInGB:     1,
						StorageClass: "ssd",
						VolumeMode:   aerospikev1alpha1.AerospikeVolumeModeFilesystem,
					},
				},
			},
			AerospikeAccessControl: &aerospikev1alpha1.AerospikeAccessControlSpec{
				Users: []aerospikev1alpha1.AerospikeUserSpec{
					{
						Name:       "admin",
						SecretName: authSecretName,
						Roles: []string{
							"sys-admin",
							"user-admin",
						},
					},
				},
			},
			AerospikeConfigSecret: aerospikev1alpha1.AerospikeConfigSecretSpec{
				SecretName: tlsSecretName,
				MountPath:  "/etc/aerospike/secret",
			},
			MultiPodPerHost: true,
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    cpu,
					corev1.ResourceMemory: mem,
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    cpu,
					corev1.ResourceMemory: mem,
				},
			},
			AerospikeConfig: aerospikev1alpha1.Values{
				"service": map[string]interface{}{
					"feature-key-file": "/etc/aerospike/secret/features.conf",
				},
				"security": map[string]interface{}{
					"enable-security": true,
				},
				"network": map[string]interface{}{
					"service": map[string]interface{}{
						"tls-name":                "bob-cluster-a",
						"tls-authenticate-client": "any",
					},
					"heartbeat": map[string]interface{}{
						"tls-name": "bob-cluster-b",
					},
					"fabric": map[string]interface{}{
						"tls-name": "bob-cluster-c",
					},
					"tls": []map[string]interface{}{
						{
							"name":      "bob-cluster-a",
							"cert-file": "/etc/aerospike/secret/svc_cluster_chain.pem",
							"key-file":  "/etc/aerospike/secret/svc_key.pem",
							"ca-file":   "/etc/aerospike/secret/cacert.pem",
						},
						{
							"name":      "bob-cluster-b",
							"cert-file": "/etc/aerospike/secret/hb_cluster_chain.pem",
							"key-file":  "/etc/aerospike/secret/hb_key.pem",
							"ca-file":   "/etc/aerospike/secret/cacert.pem",
						},
						{
							"name":      "bob-cluster-c",
							"cert-file": "/etc/aerospike/secret/fb_cluster_chain.pem",
							"key-file":  "/etc/aerospike/secret/fb_key.pem",
							"ca-file":   "/etc/aerospike/secret/cacert.pem",
						},
					},
				},
				"namespaces": []interface{}{
					map[string]interface{}{
						"name":               "test",
						"memory-size":        1000955200,
						"replication-factor": 2,
						"storage-engine": map[string]interface{}{
							"type":    "device",
							"devices": []interface{}{"/test/dev/xvdf"},
						},
					},
				},
			},
		},
	}
	return aeroCluster
}

func createDummyRackAwareAerospikeCluster(clusterNamespacedName types.NamespacedName, size int32) *aerospikev1alpha1.AerospikeCluster {
	// Will be used in Update also
	aeroCluster := createDummyAerospikeCluster(clusterNamespacedName, 2)
	// This needs to be changed based on setup. update zone, region, nodeName according to setup
	racks := []aerospikev1alpha1.Rack{{ID: 1}}
	rackConf := aerospikev1alpha1.RackConfig{Racks: racks}
	aeroCluster.Spec.RackConfig = rackConf
	return aeroCluster
}

func createDummyAerospikeCluster(clusterNamespacedName types.NamespacedName, size int32) *aerospikev1alpha1.AerospikeCluster {
	return createDummyAerospikeClusterWithOption(clusterNamespacedName, size, true)
}

var defaultProtofdmax int64 = 15000

func createDummyAerospikeClusterWithOption(clusterNamespacedName types.NamespacedName, size int32, cascadeDelete bool) *aerospikev1alpha1.AerospikeCluster {
	mem := resource.MustParse("1Gi")
	cpu := resource.MustParse("200m")
	// create Aerospike custom resource
	aeroCluster := &aerospikev1alpha1.AerospikeCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterNamespacedName.Name,
			Namespace: clusterNamespacedName.Namespace,
		},
		Spec: aerospikev1alpha1.AerospikeClusterSpec{
			Size:  size,
			Image: latestClusterImage,
			Storage: aerospikev1alpha1.AerospikeStorageSpec{
				BlockVolumePolicy: aerospikev1alpha1.AerospikePersistentVolumePolicySpec{
					InputCascadeDelete: &cascadeDeleteFalse,
				},
				FileSystemVolumePolicy: aerospikev1alpha1.AerospikePersistentVolumePolicySpec{
					InputInitMethod:    &aerospikeVolumeInitMethodDeleteFiles,
					InputCascadeDelete: &cascadeDeleteFalse,
				},
				Volumes: []aerospikev1alpha1.AerospikePersistentVolumeSpec{
					{
						Path:         "/test/dev/xvdf",
						SizeInGB:     1,
						StorageClass: "ssd",
						VolumeMode:   aerospikev1alpha1.AerospikeVolumeModeBlock,
					},
					{
						Path:         "/opt/aerospike",
						SizeInGB:     1,
						StorageClass: "ssd",
						VolumeMode:   aerospikev1alpha1.AerospikeVolumeModeFilesystem,
					},
				},
			},
			AerospikeAccessControl: &aerospikev1alpha1.AerospikeAccessControlSpec{
				Users: []aerospikev1alpha1.AerospikeUserSpec{
					{
						Name:       "admin",
						SecretName: authSecretName,
						Roles: []string{
							"sys-admin",
							"user-admin",
							"read-write",
						},
					},
				},
			},
			AerospikeConfigSecret: aerospikev1alpha1.AerospikeConfigSecretSpec{
				SecretName: tlsSecretName,
				MountPath:  "/etc/aerospike/secret",
			},
			MultiPodPerHost: true,
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    cpu,
					corev1.ResourceMemory: mem,
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    cpu,
					corev1.ResourceMemory: mem,
				},
			},
			AerospikeConfig: aerospikev1alpha1.Values{
				"service": map[string]interface{}{
					"feature-key-file": "/etc/aerospike/secret/features.conf",
					"proto-fd-max":     defaultProtofdmax,
				},
				"security": map[string]interface{}{
					"enable-security": true,
				},
				"namespaces": []interface{}{
					map[string]interface{}{
						"name":               "test",
						"memory-size":        1000955200,
						"replication-factor": 1,
						"storage-engine": map[string]interface{}{
							"type":    "device",
							"devices": []interface{}{"/test/dev/xvdf"},
						},
					},
				},
			},
		},
	}
	return aeroCluster
}

// feature-key file needed
func createBasicTLSCluster(clusterNamespacedName types.NamespacedName, size int32) *aerospikev1alpha1.AerospikeCluster {
	mem := resource.MustParse("1Gi")
	cpu := resource.MustParse("200m")
	// create Aerospike custom resource
	aeroCluster := &aerospikev1alpha1.AerospikeCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterNamespacedName.Name,
			Namespace: clusterNamespacedName.Namespace,
		},
		Spec: aerospikev1alpha1.AerospikeClusterSpec{
			Size:  size,
			Image: latestClusterImage,
			Storage: aerospikev1alpha1.AerospikeStorageSpec{
				BlockVolumePolicy: aerospikev1alpha1.AerospikePersistentVolumePolicySpec{
					InputCascadeDelete: &cascadeDeleteTrue,
				},
				FileSystemVolumePolicy: aerospikev1alpha1.AerospikePersistentVolumePolicySpec{
					InputInitMethod:    &aerospikeVolumeInitMethodDeleteFiles,
					InputCascadeDelete: &cascadeDeleteTrue,
				},
			},
			AerospikeAccessControl: &aerospikev1alpha1.AerospikeAccessControlSpec{
				Users: []aerospikev1alpha1.AerospikeUserSpec{
					{
						Name:       "admin",
						SecretName: authSecretName,
						Roles: []string{
							"sys-admin",
							"user-admin",
						},
					},
				},
			},
			AerospikeConfigSecret: aerospikev1alpha1.AerospikeConfigSecretSpec{
				SecretName: tlsSecretName,
				MountPath:  "/etc/aerospike/secret",
			},
			MultiPodPerHost: true,
			Resources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    cpu,
					corev1.ResourceMemory: mem,
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    cpu,
					corev1.ResourceMemory: mem,
				},
			},
			AerospikeConfig: aerospikev1alpha1.Values{
				"service": map[string]interface{}{
					"feature-key-file": "/etc/aerospike/secret/features.conf",
				},
				"security": map[string]interface{}{
					"enable-security": true,
				},
				"network": map[string]interface{}{
					"service": map[string]interface{}{
						"tls-name":                "bob-cluster-a",
						"tls-authenticate-client": "any",
					},
					"heartbeat": map[string]interface{}{
						"tls-name": "bob-cluster-b",
					},
					"fabric": map[string]interface{}{
						"tls-name": "bob-cluster-c",
					},
					"tls": []map[string]interface{}{
						{
							"name":      "bob-cluster-a",
							"cert-file": "/etc/aerospike/secret/svc_cluster_chain.pem",
							"key-file":  "/etc/aerospike/secret/svc_key.pem",
							"ca-file":   "/etc/aerospike/secret/cacert.pem",
						},
						{
							"name":      "bob-cluster-b",
							"cert-file": "/etc/aerospike/secret/hb_cluster_chain.pem",
							"key-file":  "/etc/aerospike/secret/hb_key.pem",
							"ca-file":   "/etc/aerospike/secret/cacert.pem",
						},
						{
							"name":      "bob-cluster-c",
							"cert-file": "/etc/aerospike/secret/fb_cluster_chain.pem",
							"key-file":  "/etc/aerospike/secret/fb_key.pem",
							"ca-file":   "/etc/aerospike/secret/cacert.pem",
						},
					},
				},
			},
		},
	}
	return aeroCluster
}

func createSSDStorageCluster(clusterNamespacedName types.NamespacedName, size int32, repFact int32, multiPodPerHost bool) *aerospikev1alpha1.AerospikeCluster {
	aeroCluster := createBasicTLSCluster(clusterNamespacedName, size)
	aeroCluster.Spec.MultiPodPerHost = multiPodPerHost
	aeroCluster.Spec.Storage.Volumes = []aerospikev1alpha1.AerospikePersistentVolumeSpec{
		{
			Path:         "/test/dev/xvdf",
			SizeInGB:     1,
			StorageClass: "ssd",
			VolumeMode:   aerospikev1alpha1.AerospikeVolumeModeBlock,
		},
		{
			Path:         "/opt/aerospike",
			SizeInGB:     1,
			StorageClass: "ssd",
			VolumeMode:   aerospikev1alpha1.AerospikeVolumeModeFilesystem,
		},
	}

	aeroCluster.Spec.AerospikeConfig["namespaces"] = []interface{}{
		map[string]interface{}{
			"name":               "test",
			"memory-size":        2000955200,
			"replication-factor": repFact,
			"storage-engine": map[string]interface{}{
				"type":    "device",
				"devices": []interface{}{"/test/dev/xvdf"},
			},
		},
	}
	return aeroCluster
}

func createHDDAndDataInMemStorageCluster(clusterNamespacedName types.NamespacedName, size int32, repFact int32, multiPodPerHost bool) *aerospikev1alpha1.AerospikeCluster {
	aeroCluster := createBasicTLSCluster(clusterNamespacedName, size)
	aeroCluster.Spec.MultiPodPerHost = multiPodPerHost
	aeroCluster.Spec.Storage.Volumes = []aerospikev1alpha1.AerospikePersistentVolumeSpec{
		{
			Path:         "/opt/aerospike",
			SizeInGB:     1,
			StorageClass: "ssd",
			VolumeMode:   aerospikev1alpha1.AerospikeVolumeModeFilesystem,
		}, {
			Path:         "/opt/aerospike/data",
			SizeInGB:     1,
			StorageClass: "ssd",
			VolumeMode:   aerospikev1alpha1.AerospikeVolumeModeFilesystem,
		},
	}

	aeroCluster.Spec.AerospikeConfig["namespaces"] = []interface{}{
		map[string]interface{}{
			"name":               "test",
			"memory-size":        2000955200,
			"replication-factor": repFact,
			"storage-engine": map[string]interface{}{
				"type":           "device",
				"files":          []interface{}{"/opt/aerospike/data/test.dat"},
				"filesize":       2000955200,
				"data-in-memory": true,
			},
		},
	}
	return aeroCluster
}

func createHDDAndDataInIndexStorageCluster(clusterNamespacedName types.NamespacedName, size int32, repFact int32, multiPodPerHost bool) *aerospikev1alpha1.AerospikeCluster {
	aeroCluster := createBasicTLSCluster(clusterNamespacedName, size)
	aeroCluster.Spec.MultiPodPerHost = multiPodPerHost
	aeroCluster.Spec.Storage.Volumes = []aerospikev1alpha1.AerospikePersistentVolumeSpec{
		{
			Path:         "/dev/xvdf1",
			SizeInGB:     1,
			StorageClass: "ssd",
			VolumeMode:   aerospikev1alpha1.AerospikeVolumeModeBlock,
		},
		{
			Path:         "/opt/aerospike",
			SizeInGB:     1,
			StorageClass: "ssd",
			VolumeMode:   aerospikev1alpha1.AerospikeVolumeModeFilesystem,
		},
		{
			Path:         "/opt/aerospike/data",
			SizeInGB:     1,
			StorageClass: "ssd",
			VolumeMode:   aerospikev1alpha1.AerospikeVolumeModeFilesystem,
		},
	}
	aeroCluster.Spec.AerospikeConfig["namespaces"] = []interface{}{
		map[string]interface{}{
			"name":               "test",
			"memory-size":        2000955200,
			"single-bin":         true,
			"data-in-index":      true,
			"replication-factor": repFact,
			"storage-engine": map[string]interface{}{
				"type":           "device",
				"files":          []interface{}{"/opt/aerospike/data/test.dat"},
				"filesize":       2000955200,
				"data-in-memory": true,
			},
		},
	}
	return aeroCluster
}

func createDataInMemWithoutPersistentStorageCluster(clusterNamespacedName types.NamespacedName, size int32, repFact int32, multiPodPerHost bool) *aerospikev1alpha1.AerospikeCluster {
	aeroCluster := createBasicTLSCluster(clusterNamespacedName, size)
	aeroCluster.Spec.MultiPodPerHost = multiPodPerHost
	aeroCluster.Spec.Storage.Volumes = []aerospikev1alpha1.AerospikePersistentVolumeSpec{
		{
			Path:         "/opt/aerospike",
			SizeInGB:     1,
			StorageClass: "ssd",
			VolumeMode:   aerospikev1alpha1.AerospikeVolumeModeFilesystem,
		},
	}
	aeroCluster.Spec.AerospikeConfig["namespaces"] = []interface{}{
		map[string]interface{}{
			"name":               "test",
			"memory-size":        2000955200,
			"replication-factor": repFact,
			"storage-engine": map[string]interface{}{
				"type": "memory",
			},
		},
	}

	return aeroCluster
}

func createShadowDeviceStorageCluster(clusterNamespacedName types.NamespacedName, size int32, repFact int32, multiPodPerHost bool) *aerospikev1alpha1.AerospikeCluster {
	aeroCluster := createBasicTLSCluster(clusterNamespacedName, size)
	aeroCluster.Spec.MultiPodPerHost = multiPodPerHost
	aeroCluster.Spec.Storage.Volumes = []aerospikev1alpha1.AerospikePersistentVolumeSpec{
		{
			Path:         "/test/dev/xvdf",
			SizeInGB:     1,
			StorageClass: "ssd",
			VolumeMode:   aerospikev1alpha1.AerospikeVolumeModeBlock,
		},
		{
			Path:         "/dev/nvme0n1",
			SizeInGB:     1,
			StorageClass: "local-ssd",
			VolumeMode:   aerospikev1alpha1.AerospikeVolumeModeBlock,
		},
		{
			Path:         "/opt/aerospike",
			SizeInGB:     1,
			StorageClass: "ssd",
			VolumeMode:   aerospikev1alpha1.AerospikeVolumeModeFilesystem,
		},
	}

	aeroCluster.Spec.AerospikeConfig["namespaces"] = []interface{}{
		map[string]interface{}{
			"name":               "test",
			"memory-size":        2000955200,
			"replication-factor": repFact,
			"storage-engine": map[string]interface{}{
				"type": "device",
				"devices": []interface{}{"/dev/nvme0n1	/test/dev/xvdf"},
			},
		},
	}
	return aeroCluster
}

func createPMEMStorageCluster(clusterNamespacedName types.NamespacedName, size int32, repFact int32, multiPodPerHost bool) *aerospikev1alpha1.AerospikeCluster {
	return nil
}

func aerospikeClusterCreateUpdateWithTO(desired *aerospikev1alpha1.AerospikeCluster, ctx *framework.TestCtx, retryInterval, timeout time.Duration, t *testing.T) error {
	current := &aerospikev1alpha1.AerospikeCluster{}
	err := framework.Global.Client.Get(context.TODO(), types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, current)
	if err != nil {
		// Deploy the cluster.
		t.Logf("Deploying cluster at %v", time.Now().Format(time.RFC850))
		if err := deployClusterWithTO(t, framework.Global, ctx, desired, retryInterval, timeout); err != nil {
			return err
		}
		t.Logf("Deployed cluster at %v", time.Now().Format(time.RFC850))
		return nil
	}
	// Apply the update.
	if desired.Spec.AerospikeAccessControl != nil {
		current.Spec.AerospikeAccessControl = &aerospikev1alpha1.AerospikeAccessControlSpec{}
		lib.DeepCopy(&current.Spec, &desired.Spec)
	} else {
		current.Spec.AerospikeAccessControl = nil
	}
	lib.DeepCopy(&current.Spec.AerospikeConfig, &desired.Spec.AerospikeConfig)

	err = framework.Global.Client.Update(context.TODO(), current)
	if err != nil {
		return err
	}

	return waitForAerospikeCluster(t, framework.Global, desired, int(desired.Spec.Size), retryInterval, timeout)
}

func aerospikeClusterCreateUpdate(desired *aerospikev1alpha1.AerospikeCluster, ctx *framework.TestCtx, t *testing.T) error {
	return aerospikeClusterCreateUpdateWithTO(desired, ctx, retryInterval, getTimeout(1), t)
}
