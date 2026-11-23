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
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// NewKubernetesSource creates the cluster-wide read-only inventory source.
// The caller's identity is the authorizer's own ServiceAccount: tenant
// credentials are never used for Kubernetes reads.
func NewKubernetesSource(config *rest.Config) (Source, error) {
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &kubernetesSource{client: client}, nil
}

type kubernetesSource struct {
	client dynamic.Interface
}

func gvr(kind Kind) schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "osac.openshift.io",
		Version:  "v1alpha1",
		Resource: string(kind),
	}
}

func (s *kubernetesSource) List(ctx context.Context, kind Kind) ([]Object, string, error) {
	list, err := s.client.Resource(gvr(kind)).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, "", err
	}
	objects := make([]Object, 0, len(list.Items))
	for index := range list.Items {
		object, ok := toObject(kind, &list.Items[index])
		if ok {
			objects = append(objects, object)
		}
	}
	return objects, list.GetResourceVersion(), nil
}

func (s *kubernetesSource) Watch(ctx context.Context, kind Kind, resourceVersion string) (Watch, error) {
	inner, err := s.client.Resource(gvr(kind)).Watch(ctx, metav1.ListOptions{ResourceVersion: resourceVersion})
	if err != nil {
		return nil, err
	}
	result := &kubernetesWatch{inner: inner, events: make(chan Event)}
	go result.forward(kind)
	return result, nil
}

type kubernetesWatch struct {
	inner  watch.Interface
	events chan Event
}

func (w *kubernetesWatch) ResultChan() <-chan Event { return w.events }

func (w *kubernetesWatch) Stop() {
	w.inner.Stop()
}

func (w *kubernetesWatch) forward(kind Kind) {
	defer close(w.events)
	for event := range w.inner.ResultChan() {
		unstructuredObject, ok := event.Object.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		object, ok := toObject(kind, unstructuredObject)
		if !ok {
			// The resource no longer belongs in any scope, so the only correct
			// action is to forget it.
			w.events <- Event{Type: EventDelete, UID: string(unstructuredObject.GetUID())}
			continue
		}
		eventType := EventUpdate
		switch event.Type {
		case watch.Added:
			eventType = EventAdd
		case watch.Deleted:
			eventType = EventDelete
		}
		w.events <- Event{Type: eventType, UID: object.UID, Object: object}
	}
}

// toObject normalizes one unstructured OSAC resource. A resource without a
// tenant annotation, or without the reference the scope needs, is skipped: it
// has no scope to contribute to.
func toObject(kind Kind, unstructuredObject *unstructured.Unstructured) (Object, bool) {
	annotations := unstructuredObject.GetAnnotations()
	tenant := annotations[TenantAnnotation]
	if tenant == "" {
		return Object{}, false
	}
	project, hasProject := annotations[ProjectAnnotation]
	object := Object{
		Kind:       kind,
		UID:        string(unstructuredObject.GetUID()),
		Tenant:     tenant,
		Project:    project,
		HasProject: hasProject,
	}
	switch kind {
	case KindClusterOrders:
		cluster, _, _ := unstructured.NestedString(unstructuredObject.Object,
			"status", "clusterReference", "hostedClusterName")
		namespace, _, _ := unstructured.NestedString(unstructuredObject.Object,
			"status", "clusterReference", "namespace")
		if cluster == "" && namespace == "" {
			return Object{}, false
		}
		object.HostedCluster = cluster
		object.HostedNamespace = namespace
	case KindComputeInstances:
		vmName, _, _ := unstructured.NestedString(unstructuredObject.Object,
			"status", "virtualMachineReference", "kubeVirtVirtualMachineName")
		vmNamespace, _, _ := unstructured.NestedString(unstructuredObject.Object,
			"status", "virtualMachineReference", "namespace")
		if vmName == "" {
			return Object{}, false
		}
		object.VMName = vmName
		object.VMNamespace = vmNamespace
		object.TenantNamespace, _, _ = unstructured.NestedString(unstructuredObject.Object,
			"status", "tenantReference", "namespace")
		if object.TenantNamespace == "" {
			object.TenantNamespace = unstructuredObject.GetNamespace()
		}
	case KindVolumes:
		object.Namespace = unstructuredObject.GetNamespace()
		object.Name = unstructuredObject.GetName()
		if object.Name == "" {
			return Object{}, false
		}
	default:
		return Object{}, false
	}
	return object, true
}
