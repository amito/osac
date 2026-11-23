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
	"reflect"
	"testing"
)

func order(tenant, project string, hasProject bool, cluster, namespace string) Object {
	return Object{
		Kind: KindClusterOrders, Tenant: tenant, Project: project, HasProject: hasProject,
		HostedCluster: cluster, HostedNamespace: namespace,
	}
}

func instance(tenant, project string, hasProject bool, vmNamespace, vmName, tenantNamespace string) Object {
	return Object{
		Kind: KindComputeInstances, Tenant: tenant, Project: project, HasProject: hasProject,
		VMNamespace: vmNamespace, VMName: vmName, TenantNamespace: tenantNamespace,
	}
}

func volume(tenant, project string, hasProject bool, namespace, name string) Object {
	return Object{
		Kind: KindVolumes, Tenant: tenant, Project: project, HasProject: hasProject,
		Namespace: namespace, Name: name,
	}
}

func TestBuildSnapshotOrganizationScope(t *testing.T) {
	snapshot := BuildSnapshot([]Object{
		order("acme", "", false, "acme-1", "acme-cp-1"),
		instance("acme", "", false, "acme-vnet", "vm-1", "acme-tenant"),
		volume("acme", "", false, "acme-vnet", "vol-1"),
		// Never appears: no tenant attribution.
		instance("", "", false, "orphan", "vm-x", "orphan"),
	})
	org := snapshot.Organizations["acme"]
	if org == nil {
		t.Fatal("expected an acme organization")
	}
	wantNamespaces := []string{"acme-cp-1", "acme-tenant", "acme-vnet"}
	if !reflect.DeepEqual(org.Namespaces, wantNamespaces) {
		t.Errorf("namespaces: want %v, got %v", wantNamespaces, org.Namespaces)
	}
	if !reflect.DeepEqual(org.Clusters, []string{"acme-1"}) {
		t.Errorf("clusters: want [acme-1], got %v", org.Clusters)
	}
	if _, ok := snapshot.Organizations[""]; ok {
		t.Error("an unattributed organization must not exist")
	}
}

func TestBuildSnapshotRemovesManagementAndCollidingClusters(t *testing.T) {
	snapshot := BuildSnapshot([]Object{
		// A cluster named management is removed from every scope, including its
		// hosted namespace.
		order("acme", "", false, ManagementCluster, "acme-poisoned"),
		// The same cluster name under two tenants is removed from both.
		order("acme", "", false, "shared", "acme-shared-cp"),
		order("globex", "", false, "shared", "globex-shared-cp"),
		order("globex", "", false, "globex-1", "globex-cp-1"),
	})
	acme := snapshot.Organizations["acme"]
	if acme == nil {
		t.Fatal("expected an acme organization")
	}
	if len(acme.Clusters) != 0 {
		t.Errorf("acme clusters: want none, got %v", acme.Clusters)
	}
	if len(acme.Namespaces) != 0 {
		t.Errorf("acme namespaces: want none, got %v", acme.Namespaces)
	}
	globex := snapshot.Organizations["globex"]
	if !reflect.DeepEqual(globex.Clusters, []string{"globex-1"}) {
		t.Errorf("globex clusters: want [globex-1], got %v", globex.Clusters)
	}
	if !reflect.DeepEqual(globex.Namespaces, []string{"globex-cp-1"}) {
		t.Errorf("globex namespaces: want [globex-cp-1], got %v", globex.Namespaces)
	}
}

func TestBuildSnapshotProjects(t *testing.T) {
	snapshot := BuildSnapshot([]Object{
		order("acme", "team-a", true, "acme-1", "acme-cp-1"),
		order("acme", "", true, "acme-2", "acme-cp-2"),
		instance("acme", "team-a", true, "vnet-1", "vm-1", "tenant-1"),
		instance("acme", "team-a", true, "vnet-2", "vm-2", "tenant-1"),
		volume("acme", "team-a", true, "vnet-1", "vol-1"),
		// Absent annotation: visible to the organization only.
		order("acme", "", false, "acme-unannotated", "acme-cp-unannotated"),
		instance("acme", "", false, "vnet-x", "vm-x", "tenant-x"),
	})
	acme := snapshot.Organizations["acme"]
	if _, ok := acme.Projects["team-a"]; !ok {
		t.Fatal("expected project team-a")
	}
	teamA := acme.Projects["team-a"]
	if !reflect.DeepEqual(teamA.ClusterNames, []string{"acme-1"}) {
		t.Errorf("team-a clusters: got %v", teamA.ClusterNames)
	}
	if !reflect.DeepEqual(teamA.HostedNamespaces, []string{"acme-cp-1"}) {
		t.Errorf("team-a hosted namespaces: got %v", teamA.HostedNamespaces)
	}
	if !reflect.DeepEqual(teamA.VMNamespaces, []string{"vnet-1", "vnet-2"}) {
		t.Errorf("team-a vm namespaces: got %v", teamA.VMNamespaces)
	}
	if !reflect.DeepEqual(teamA.VMNames, []string{"vm-1", "vm-2"}) {
		t.Errorf("team-a vm names: got %v", teamA.VMNames)
	}
	if !reflect.DeepEqual(teamA.VolumeNames, []string{"vol-1"}) {
		t.Errorf("team-a volume names: got %v", teamA.VolumeNames)
	}
	if !reflect.DeepEqual(teamA.TenantNamespaces, []string{"tenant-1", "vnet-1"}) {
		t.Errorf("team-a tenant namespaces: got %v", teamA.TenantNamespaces)
	}
	// The empty annotation is the default project.
	if _, ok := acme.Projects[""]; !ok {
		t.Fatal("expected the default project")
	}
	if !reflect.DeepEqual(acme.Projects[""].ClusterNames, []string{"acme-2"}) {
		t.Errorf("default project clusters: got %v", acme.Projects[""].ClusterNames)
	}
	// The absent annotation never reaches a project, but stays in the org.
	for name, project := range acme.Projects {
		for _, namespace := range project.HostedNamespaces {
			if namespace == "acme-cp-unannotated" {
				t.Errorf("project %q must not see an unannotated resource", name)
			}
		}
	}
	if !contains(acme.Namespaces, "acme-cp-unannotated") {
		t.Errorf("the organization must still see the unannotated namespace: %v", acme.Namespaces)
	}
}

func TestBuildSnapshotProjectsArePerTenant(t *testing.T) {
	snapshot := BuildSnapshot([]Object{
		order("acme", "team-a", true, "acme-1", "acme-cp"),
		order("globex", "team-a", true, "globex-1", "globex-cp"),
	})
	if got := snapshot.Organizations["acme"].Projects["team-a"].ClusterNames; !reflect.DeepEqual(got, []string{"acme-1"}) {
		t.Errorf("acme team-a: got %v", got)
	}
	if got := snapshot.Organizations["globex"].Projects["team-a"].ClusterNames; !reflect.DeepEqual(got, []string{"globex-1"}) {
		t.Errorf("globex team-a: got %v", got)
	}
}

func TestBuildSnapshotPoisonedOrderIsNotAProjectResource(t *testing.T) {
	snapshot := BuildSnapshot([]Object{
		order("acme", "team-a", true, ManagementCluster, "acme-poisoned"),
		order("globex", "team-a", true, ManagementCluster, "globex-poisoned"),
	})
	for _, tenant := range []string{"acme", "globex"} {
		project := snapshot.Organizations[tenant].Projects["team-a"]
		if project == nil {
			continue
		}
		if len(project.ClusterNames) != 0 || len(project.HostedNamespaces) != 0 {
			t.Errorf("%s project must not see a poisoned cluster: %+v", tenant, project)
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
