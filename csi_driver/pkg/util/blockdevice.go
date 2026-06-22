package util

import (
	"context"
	"fmt"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

type BlockDevice struct {
	Name       string
	Node       string
	MountPoint string
	Serial     string
	Class      string
	Free       int64
}

var blockDeviceGVR = schema.GroupVersionResource{
	Group:    "freeport.local",
	Version:  "v1alpha1",
	Resource: "blockdevices",
}

func getKubeConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	kubeconfig := filepath.Join(homedir.HomeDir(), ".kube", "config")
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func GetAvailableDevices(ctx context.Context, desiredStorageClass string) ([]BlockDevice, error) {
	cfg, err := getKubeConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get kube config: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	// Cluster-scoped resource — no namespace.
	list, err := dynamicClient.Resource(blockDeviceGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list BlockDevices: %w", err)
	}
	Log.Info("listed BlockDevices", "count", len(list.Items))

	devices := make([]BlockDevice, 0, len(list.Items))
	for _, obj := range list.Items {
		node, _, _ := unstructured.NestedString(obj.Object, "spec", "node")
		mountpoint, _, _ := unstructured.NestedString(obj.Object, "spec", "mountpoint")
		serial, _, _ := unstructured.NestedString(obj.Object, "spec", "serial")
		class, _, _ := unstructured.NestedString(obj.Object, "spec", "class")
		free, _, _ := unstructured.NestedInt64(obj.Object, "spec", "free")
		// REVISIT: use parameters.storageClass for consistency semantic
		// deviceStorageClass, ok, _ := unstructured.NestedString(obj.Object, "spec", "parameters.storageClassName")
		// if !ok {
		// 	Log.Error("No storage class found: ", "serial", serial)
		// }
		deviceStorageClass := class

		if deviceStorageClass == desiredStorageClass {
			devices = append(devices, BlockDevice{
				Name:       obj.GetName(),
				Node:       node,
				MountPoint: mountpoint,
				Serial:     serial,
				Class:      class,
				Free:       free,
			})
		}
	}

	return devices, nil
}
