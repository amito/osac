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

func renderScope(scope Scope) []string {
	if len(scope.Branches) == 0 {
		return nil
	}
	result := make([]string, len(scope.Branches))
	for i, branch := range scope.Branches {
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
			got := renderScope(OrganizationScope(test.org))
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
