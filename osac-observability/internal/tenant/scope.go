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
	"regexp"
	"sort"
	"strings"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/osac-project/osac/osac-observability/internal/inventory"
)

const (
	// metricNameLabel is the reserved label that carries a series' metric name.
	metricNameLabel = "__name__"
	// persistentVolumeFamily is excluded from the project predicate: a
	// PersistentVolume is cluster-scoped, so no single project owns it. The
	// prefix deliberately does not match kube_persistentvolumeclaim_*, which is
	// a namespaced, project-owned family.
	persistentVolumeFamily = "kube_persistentvolume_"
)

var (
	// vmFamilyPrefixes are the metric families whose name label is the VM name.
	vmFamilyPrefixes = []string{"kubevirt_vmi_", "kubevirt_vm_"}
	// claimFamilyPrefixes are the families whose claim label is the volume name.
	claimFamilyPrefixes = []string{
		"kube_persistentvolumeclaim_",
		"kube_pod_spec_volumes_persistentvolumeclaims_info",
	}
)

// Branch is one conjunction of label matchers. A scope admits a series when it
// matches any one branch.
type Branch []*labels.Matcher

// BranchSpec is one branch of a scope before the per-selector family rules are
// applied. Families, when set, restricts the branch to those metric families;
// nil admits every family.
type BranchSpec struct {
	Matchers []*labels.Matcher
	Families []string
}

// Scope is the caller's set of admitted branches. An empty scope matches
// nothing; it is never rendered as an unrestricted selector.
type Scope struct {
	Branches []BranchSpec
}

// Empty reports whether the scope admits nothing.
func (s Scope) Empty() bool {
	return len(s.Branches) == 0
}

// BranchesFor resolves the scope for one selector. metric is the selector's
// effective metric name (empty when the caller's __name__ matcher is not an
// equality) and nameConstrained reports whether the selector constrains
// __name__ at all.
//
// A branch with families applies only to those families. A caller that does not
// constrain __name__ gets each family branch narrowed by a family __name__
// regex, so an unconstrained selector cannot widen a family branch. An explicit
// kube_persistentvolume_* request admits nothing at all.
func (s Scope) BranchesFor(metric string, nameConstrained bool) []Branch {
	if nameConstrained && strings.HasPrefix(metric, persistentVolumeFamily) {
		return nil
	}
	branches := make([]Branch, 0, len(s.Branches))
	for _, spec := range s.Branches {
		if spec.Families != nil && nameConstrained && !familyMatches(metric, spec.Families) {
			continue
		}
		branch := make(Branch, len(spec.Matchers), len(spec.Matchers)+1)
		copy(branch, spec.Matchers)
		if spec.Families != nil && !nameConstrained {
			branch = append(branch, labels.MustNewMatcher(labels.MatchRegexp,
				metricNameLabel, familyRegex(spec.Families)))
		}
		branches = append(branches, branch)
	}
	if len(branches) == 0 {
		return nil
	}
	return branches
}

// OrganizationScope builds the two-branch organization predicate: the
// organization's namespaces on the management cluster, and the organization's
// hosted clusters. Values that would make a branch match everything are never
// emitted: an organization with no resources yields an empty scope.
func OrganizationScope(org *inventory.Organization) Scope {
	if org == nil {
		return Scope{}
	}
	var scope Scope
	if regex := RegexFor(org.Namespaces); regex != "" {
		scope.Branches = append(scope.Branches, BranchSpec{Matchers: []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, "cluster", inventory.ManagementCluster),
			labels.MustNewMatcher(labels.MatchRegexp, "namespace", regex),
		}})
	}
	var clusters []string
	for _, cluster := range org.Clusters {
		if cluster == "" || cluster == inventory.ManagementCluster {
			continue
		}
		clusters = append(clusters, cluster)
	}
	if regex := RegexFor(clusters); regex != "" {
		scope.Branches = append(scope.Branches, BranchSpec{Matchers: []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchRegexp, "cluster", regex),
		}})
	}
	return scope
}

// ProjectScope builds the three-branch project predicate: the project's VM and
// volume resources on the management cluster (with the family matcher), the
// project's hosted control-plane and working namespaces, and the project's
// tenant clusters. Each branch is omitted when the values it needs are absent,
// and a project with no resources yields an empty scope.
func ProjectScope(project *inventory.Project) Scope {
	if project == nil {
		return Scope{}
	}
	var scope Scope
	// (a) The project's VMs, addressed by the labels kubevirt exposes: the VM
	// namespace and the VM name, never the tenant namespace.
	if namespaces := RegexFor(project.VMNamespaces); namespaces != "" {
		if names := RegexFor(project.VMNames); names != "" {
			scope.Branches = append(scope.Branches, BranchSpec{
				Matchers: []*labels.Matcher{
					labels.MustNewMatcher(labels.MatchEqual, "cluster", inventory.ManagementCluster),
					labels.MustNewMatcher(labels.MatchRegexp, "namespace", namespaces),
					labels.MustNewMatcher(labels.MatchRegexp, "name", names),
				},
				Families: vmFamilyPrefixes,
			})
		}
	}
	// (a) The project's volumes, addressed by the claim label. A claim series
	// can live in a tenant namespace as well as in the VM namespace.
	if namespaces := RegexFor(unionSorted(project.VMNamespaces, project.TenantNamespaces)); namespaces != "" {
		if volumes := RegexFor(project.VolumeNames); volumes != "" {
			scope.Branches = append(scope.Branches, BranchSpec{
				Matchers: []*labels.Matcher{
					labels.MustNewMatcher(labels.MatchEqual, "cluster", inventory.ManagementCluster),
					labels.MustNewMatcher(labels.MatchRegexp, "namespace", namespaces),
					labels.MustNewMatcher(labels.MatchRegexp, "persistentvolumeclaim", volumes),
				},
				Families: claimFamilyPrefixes,
			})
		}
	}
	// (b) The project's hosted control-plane and working namespaces.
	if hosted := RegexFor(project.HostedNamespaces); hosted != "" {
		scope.Branches = append(scope.Branches, BranchSpec{Matchers: []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, "cluster", inventory.ManagementCluster),
			labels.MustNewMatcher(labels.MatchRegexp, "namespace", hosted),
		}})
	}
	// (c) The project's tenant clusters.
	if clusters := RegexFor(project.ClusterNames); clusters != "" {
		scope.Branches = append(scope.Branches, BranchSpec{Matchers: []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchRegexp, "cluster", clusters),
		}})
	}
	return scope
}

// familyMatches reports whether metric belongs to one of the families. Family
// entries are metric-name prefixes.
func familyMatches(metric string, families []string) bool {
	for _, family := range families {
		if strings.HasPrefix(metric, family) {
			return true
		}
	}
	return false
}

// familyRegex renders the __name__ matcher for an unconstrained selector. A
// prefix family (one ending in "_") admits every metric beneath it, while a
// full metric name is matched exactly.
func familyRegex(families []string) string {
	patterns := make([]string, 0, len(families))
	for _, family := range families {
		pattern := regexp.QuoteMeta(family)
		if strings.HasSuffix(family, "_") {
			pattern += ".*"
		}
		patterns = append(patterns, pattern)
	}
	return strings.Join(patterns, "|")
}

// unionSorted returns the sorted, de-duplicated union of the groups. The order
// is deterministic so an identical project always renders identically.
func unionSorted(groups ...[]string) []string {
	seen := make(map[string]struct{})
	var values []string
	for _, group := range groups {
		for _, value := range group {
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			values = append(values, value)
		}
	}
	sort.Strings(values)
	return values
}

// RegexFor renders values as one fully anchored alternation. Every value is
// quoted, so a namespace that looks like a regular expression is matched
// literally. An empty slice yields the empty string, which callers must treat
// as "no branch" rather than as a match-all regex.
func RegexFor(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		quoted = append(quoted, regexp.QuoteMeta(value))
	}
	return strings.Join(quoted, "|")
}
