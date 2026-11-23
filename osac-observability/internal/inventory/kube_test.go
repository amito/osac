/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package inventory

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func unstructuredObject(kind Kind, annotations map[string]string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "osac.openshift.io/v1alpha1",
		"kind":       string(kind),
		"metadata": map[string]any{
			"name":      "example",
			"namespace": "example-ns",
			"uid":       "uid-1",
		},
	}}
	object.SetAnnotations(annotations)
	return object
}

func TestToObject(t *testing.T) {
	t.Run("cluster order", func(t *testing.T) {
		object := unstructuredObject(KindClusterOrders, map[string]string{
			TenantAnnotation: "acme", ProjectAnnotation: "team-a",
		})
		object.Object["status"] = map[string]any{"clusterReference": map[string]any{
			"hostedClusterName": "acme-1", "namespace": "acme-cp-1",
		}}
		got, ok := toObject(KindClusterOrders, object)
		if !ok {
			t.Fatal("expected the object to be recognized")
		}
		if got.Tenant != "acme" || got.Project != "team-a" || !got.HasProject {
			t.Errorf("unexpected attribution: %+v", got)
		}
		if got.HostedCluster != "acme-1" || got.HostedNamespace != "acme-cp-1" {
			t.Errorf("unexpected cluster reference: %+v", got)
		}
	})
	t.Run("cluster order without a reference is skipped", func(t *testing.T) {
		object := unstructuredObject(KindClusterOrders, map[string]string{TenantAnnotation: "acme"})
		if _, ok := toObject(KindClusterOrders, object); ok {
			t.Fatal("an order without a cluster reference must be skipped")
		}
	})
	t.Run("compute instance uses the virtual machine reference", func(t *testing.T) {
		object := unstructuredObject(KindComputeInstances, map[string]string{
			TenantAnnotation: "acme", ProjectAnnotation: "",
		})
		object.Object["status"] = map[string]any{
			"virtualMachineReference": map[string]any{
				"namespace": "vnet-ns", "kubeVirtVirtualMachineName": "vm-1",
			},
			"tenantReference": map[string]any{"namespace": "tenant-ns"},
		}
		got, ok := toObject(KindComputeInstances, object)
		if !ok {
			t.Fatal("expected the object to be recognized")
		}
		if got.VMNamespace != "vnet-ns" || got.VMName != "vm-1" {
			t.Errorf("unexpected vm reference: %+v", got)
		}
		if got.TenantNamespace != "tenant-ns" {
			t.Errorf("unexpected tenant namespace: %q", got.TenantNamespace)
		}
		if !got.HasProject || got.Project != "" {
			t.Errorf("an empty annotation must be the default project: %+v", got)
		}
	})
	t.Run("compute instance without a vm is skipped", func(t *testing.T) {
		object := unstructuredObject(KindComputeInstances, map[string]string{TenantAnnotation: "acme"})
		if _, ok := toObject(KindComputeInstances, object); ok {
			t.Fatal("an instance without a virtual machine must be skipped")
		}
	})
	t.Run("volume name is the pvc name", func(t *testing.T) {
		object := unstructuredObject(KindVolumes, map[string]string{TenantAnnotation: "acme"})
		got, ok := toObject(KindVolumes, object)
		if !ok {
			t.Fatal("expected the object to be recognized")
		}
		if got.Name != "example" || got.Namespace != "example-ns" {
			t.Errorf("unexpected volume: %+v", got)
		}
	})
	t.Run("missing tenant is skipped", func(t *testing.T) {
		object := unstructuredObject(KindVolumes, map[string]string{ProjectAnnotation: "team-a"})
		if _, ok := toObject(KindVolumes, object); ok {
			t.Fatal("a resource without a tenant annotation must be skipped")
		}
	})
}
