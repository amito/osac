/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package tenant

import (
	"reflect"
	"strings"
	"testing"

	"github.com/osac-project/osac/osac-observability/internal/inventory"
)

func renderBranch(branch Branch) string {
	result := ""
	for i, matcher := range branch {
		if i > 0 {
			result += ","
		}
		result += matcher.String()
	}
	return result
}

func renderScope(scope Scope, metric string, nameConstrained bool) []string {
	branches := scope.BranchesFor(metric, nameConstrained)
	if len(branches) == 0 {
		return nil
	}
	result := make([]string, len(branches))
	for i, branch := range branches {
		result[i] = renderBranch(branch)
	}
	return result
}

func TestOrganizationBranches(t *testing.T) {
	tests := []struct {
		name string
		org  *inventory.Organization
		want []string
	}{
		{
			name: "namespaces and clusters",
			org: &inventory.Organization{
				Namespaces: []string{"acme-vms"},
				Clusters:   []string{"acme-1"},
			},
			want: []string{
				`cluster="management",namespace=~"acme-vms"`,
				`cluster=~"acme-1"`,
			},
		},
		{
			name: "namespaces only",
			org:  &inventory.Organization{Namespaces: []string{"acme-vms"}},
			want: []string{`cluster="management",namespace=~"acme-vms"`},
		},
		{
			name: "clusters only",
			org:  &inventory.Organization{Clusters: []string{"acme-1"}},
			want: []string{`cluster=~"acme-1"`},
		},
		{
			name: "no resources",
			org:  &inventory.Organization{},
			want: nil,
		},
		{
			name: "nil organization",
			org:  nil,
			want: nil,
		},
		{
			name: "regex metacharacters are escaped",
			org: &inventory.Organization{
				Namespaces: []string{"a.b"},
				Clusters:   []string{"c+"},
			},
			want: []string{
				`cluster="management",namespace=~"a\\.b"`,
				`cluster=~"c\\+"`,
			},
		},
		{
			name: "management is never emitted as a hosted cluster",
			org: &inventory.Organization{
				Clusters: []string{inventory.ManagementCluster},
			},
			want: nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := renderScope(OrganizationScope(test.org), "", false)
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("want %v, got %v", test.want, got)
			}
		})
	}
}

func TestRegexFor(t *testing.T) {
	tests := []struct {
		values []string
		want   string
	}{
		{values: nil, want: ""},
		{values: []string{"a"}, want: "a"},
		{values: []string{"a", "b"}, want: "a|b"},
		{values: []string{"a.b"}, want: `a\.b`},
		{values: []string{"a|b"}, want: `a\|b`},
		{values: []string{"a+", "b"}, want: `a\+|b`},
		{values: []string{"(a)"}, want: `\(a\)`},
	}
	for _, test := range tests {
		if got := RegexFor(test.values); got != test.want {
			t.Errorf("RegexFor(%v): want %q, got %q", test.values, test.want, got)
		}
	}
}

// projectFixture is one project owning a VM namespace, a tenant namespace, a
// volume, a hosted namespace and a cluster.
func projectFixture() *inventory.Project {
	return &inventory.Project{
		VMNamespaces:     []string{"vms"},
		VMNames:          []string{"vm-1"},
		TenantNamespaces: []string{"tenant-ns"},
		VolumeNames:      []string{"vol-1"},
		HostedNamespaces: []string{"cp"},
		ClusterNames:     []string{"c-1"},
	}
}

var (
	projectVMBranches = []string{
		`cluster="management",namespace=~"vms",name=~"vm-1"`,
		`cluster="management",namespace=~"cp"`,
		`cluster=~"c-1"`,
	}
	projectClaimBranches = []string{
		`cluster="management",namespace=~"tenant-ns|vms",persistentvolumeclaim=~"vol-1"`,
		`cluster="management",namespace=~"cp"`,
		`cluster=~"c-1"`,
	}
	projectUnrelatedBranches = []string{
		`cluster="management",namespace=~"cp"`,
		`cluster=~"c-1"`,
	}
)

func TestProjectBranchesPerFamily(t *testing.T) {
	tests := []struct {
		metric string
		want   []string
	}{
		{metric: "kubevirt_vmi_cpu_usage_seconds_total", want: projectVMBranches},
		{metric: "kubevirt_vm_info", want: projectVMBranches},
		{metric: "kube_persistentvolumeclaim_status_phase", want: projectClaimBranches},
		{
			metric: "kube_pod_spec_volumes_persistentvolumeclaims_info",
			want:   projectClaimBranches,
		},
		{metric: "kube_persistentvolume_capacity_bytes", want: nil},
		{metric: "node_cpu_seconds_total", want: projectUnrelatedBranches},
		{metric: "up", want: projectUnrelatedBranches},
	}
	for _, test := range tests {
		t.Run(test.metric, func(t *testing.T) {
			got := renderScope(ProjectScope(projectFixture()), test.metric, true)
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("want %v, got %v", test.want, got)
			}
		})
	}
}

func TestProjectScopeNoClustersOmitsBranch(t *testing.T) {
	project := projectFixture()
	project.ClusterNames = nil
	scope := ProjectScope(project)
	if got := renderScope(scope, "up", true); !reflect.DeepEqual(got,
		[]string{`cluster="management",namespace=~"cp"`}) {
		t.Errorf("want the hosted branch only, got %v", got)
	}
	got := renderScope(scope, "kubevirt_vmi_x", true)
	if !reflect.DeepEqual(got, projectVMBranches[:2]) {
		t.Errorf("want the VM and hosted branches, got %v", got)
	}
	if strings.Contains(strings.Join(got, ","), `cluster=~""`) {
		t.Errorf("an empty cluster list must not be rendered: %v", got)
	}
}

func TestProjectScopeNoVMsOmitsVMBranch(t *testing.T) {
	project := projectFixture()
	project.VMNamespaces = nil
	project.VMNames = nil
	got := renderScope(ProjectScope(project), "kubevirt_vmi_x", true)
	if !reflect.DeepEqual(got, projectUnrelatedBranches) {
		t.Errorf("want the hosted and cluster branches only, got %v", got)
	}
	for _, branch := range got {
		if strings.Contains(branch, "name=~") {
			t.Errorf("the VM branch must be omitted, got %v", got)
		}
	}
}

func TestProjectScopeEmptyProjectReturnsNothing(t *testing.T) {
	for _, project := range []*inventory.Project{nil, {}} {
		scope := ProjectScope(project)
		if !scope.Empty() {
			t.Errorf("an empty project must build an empty scope: %v", renderScope(scope, "", false))
		}
		for _, selector := range []struct {
			metric      string
			constrained bool
		}{
			{metric: "", constrained: false},
			{metric: "up", constrained: true},
			{metric: "kubevirt_vmi_x", constrained: true},
		} {
			if got := renderScope(scope, selector.metric, selector.constrained); got != nil {
				t.Errorf("an empty project must admit nothing, got %v", got)
			}
		}
	}
}

func TestProjectScopeQuotesEveryValue(t *testing.T) {
	project := &inventory.Project{
		VMNamespaces:     []string{"a.*"},
		VMNames:          []string{"b|b"},
		HostedNamespaces: []string{"c+"},
		ClusterNames:     []string{"d+"},
	}
	got := renderScope(ProjectScope(project), "kubevirt_vmi_x", true)
	want := []string{
		`cluster="management",namespace=~"a\\.\\*",name=~"b\\|b"`,
		`cluster="management",namespace=~"c\\+"`,
		`cluster=~"d\\+"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("want %v, got %v", want, got)
	}
}

func TestProjectScopeDefaultProject(t *testing.T) {
	// The default project ("") has no separate representation: its resources
	// render exactly like any other project's.
	named := projectFixture()
	defaultProject := projectFixture()
	want := renderScope(ProjectScope(named), "kubevirt_vmi_x", true)
	got := renderScope(ProjectScope(defaultProject), "kubevirt_vmi_x", true)
	if !reflect.DeepEqual(got, want) || len(got) == 0 {
		t.Errorf("the default project must render like any other: want %v, got %v", want, got)
	}
}

func TestFamilyMatcherExcludesPersistentVolume(t *testing.T) {
	scope := ProjectScope(projectFixture())
	for _, metric := range []string{
		"kube_persistentvolume_capacity_bytes",
		"kube_persistentvolume_status_phase",
		"kube_persistentvolume_info",
	} {
		if got := renderScope(scope, metric, true); got != nil {
			t.Errorf("%s must match nothing, got %v", metric, got)
		}
	}
}
