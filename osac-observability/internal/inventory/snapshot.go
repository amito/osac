/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

// Package inventory keeps a read-only, cluster-wide view of the OSAC resources
// that define the tenant and project scopes. The authorizer reads Kubernetes
// with its own ServiceAccount; no tenant credential is ever used for these
// reads.
package inventory

import "sort"

const (
	// TenantAnnotation attributes a resource to an organization.
	TenantAnnotation = "osac.openshift.io/tenant"

	// ProjectAnnotation attributes a resource to a project. Its presence with
	// an empty value means the default project; its absence means the resource
	// is not project-scoped at all.
	ProjectAnnotation = "osac.openshift.io/project"

	// ManagementCluster is the reserved cluster identity of the management
	// cluster. A resource claiming it is removed from every scope.
	ManagementCluster = "management"
)

// Kind is one of the watched OSAC resource kinds.
type Kind string

const (
	KindClusterOrders    Kind = "clusterorders"
	KindComputeInstances Kind = "computeinstances"
	KindVolumes          Kind = "volumes"
)

// Object is one normalized inventory record. Only the fields of its Kind are
// populated.
type Object struct {
	Kind Kind

	Tenant     string
	Project    string
	HasProject bool

	// ClusterOrder
	HostedCluster   string
	HostedNamespace string

	// ComputeInstance
	VMNamespace     string
	VMName          string
	TenantNamespace string

	// Volume. Name is the ComputeInstance-visible name, which for a volume is
	// the backing PVC's name.
	Namespace string
	Name      string
}

// Snapshot is an immutable view of the inventory at one point in time.
type Snapshot struct {
	Organizations map[string]*Organization
}

// Organization is one tenant's organization-wide scope.
type Organization struct {
	Namespaces []string
	Clusters   []string
	Projects   map[string]*Project
}

// Project is one project's scope within its organization.
type Project struct {
	VMNamespaces     []string
	VMNames          []string
	TenantNamespaces []string
	VolumeNames      []string
	HostedNamespaces []string
	ClusterNames     []string
}

// BuildSnapshot groups normalized records into organization and project
// scopes. A ClusterOrder whose cluster identity is the management cluster, or
// whose name collides with another organization's cluster, is removed from
// every scope.
func BuildSnapshot(objects []Object) *Snapshot {
	owners := map[string]map[string]bool{}
	for _, object := range objects {
		if object.Kind != KindClusterOrders || object.HostedCluster == "" || object.Tenant == "" {
			continue
		}
		if owners[object.HostedCluster] == nil {
			owners[object.HostedCluster] = map[string]bool{}
		}
		owners[object.HostedCluster][object.Tenant] = true
	}
	poisoned := func(object Object) bool {
		if object.HostedCluster == ManagementCluster {
			return true
		}
		return len(owners[object.HostedCluster]) > 1
	}

	snapshot := &Snapshot{Organizations: map[string]*Organization{}}
	for _, object := range objects {
		if object.Tenant == "" {
			continue
		}
		org := snapshot.Organizations[object.Tenant]
		if org == nil {
			org = &Organization{Projects: map[string]*Project{}}
			snapshot.Organizations[object.Tenant] = org
		}
		// A poisoned ClusterOrder contributes nothing anywhere, but its
		// organization still exists with an empty scope.
		if object.Kind == KindClusterOrders && poisoned(object) {
			continue
		}
		switch object.Kind {
		case KindClusterOrders:
			org.Namespaces = append(org.Namespaces, object.HostedNamespace)
			org.Clusters = append(org.Clusters, object.HostedCluster)
		case KindComputeInstances:
			org.Namespaces = append(org.Namespaces, object.VMNamespace, object.TenantNamespace)
		case KindVolumes:
			org.Namespaces = append(org.Namespaces, object.Namespace)
		}
		if !object.HasProject {
			continue
		}
		project := org.Projects[object.Project]
		if project == nil {
			project = &Project{}
			org.Projects[object.Project] = project
		}
		switch object.Kind {
		case KindClusterOrders:
			project.HostedNamespaces = append(project.HostedNamespaces, object.HostedNamespace)
			project.ClusterNames = append(project.ClusterNames, object.HostedCluster)
		case KindComputeInstances:
			project.VMNamespaces = append(project.VMNamespaces, object.VMNamespace)
			project.VMNames = append(project.VMNames, object.VMName)
			project.TenantNamespaces = append(project.TenantNamespaces, object.TenantNamespace)
		case KindVolumes:
			project.VolumeNames = append(project.VolumeNames, object.Name)
			project.TenantNamespaces = append(project.TenantNamespaces, object.Namespace)
		}
	}

	for _, org := range snapshot.Organizations {
		org.Namespaces = sortUnique(org.Namespaces)
		org.Clusters = sortUnique(org.Clusters)
		for _, project := range org.Projects {
			project.VMNamespaces = sortUnique(project.VMNamespaces)
			project.VMNames = sortUnique(project.VMNames)
			project.TenantNamespaces = sortUnique(project.TenantNamespaces)
			project.VolumeNames = sortUnique(project.VolumeNames)
			project.HostedNamespaces = sortUnique(project.HostedNamespaces)
			project.ClusterNames = sortUnique(project.ClusterNames)
		}
	}
	return snapshot
}

// sortUnique drops empty values, sorts the rest and removes duplicates.
func sortUnique(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
