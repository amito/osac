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
	"strings"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/osac-project/osac/osac-observability/internal/inventory"
)

// Branch is one conjunction of label matchers. A scope admits a series when it
// matches any one branch.
type Branch []*labels.Matcher

// Scope is the caller's set of admitted branches. An empty scope matches
// nothing; it is never rendered as an unrestricted selector.
type Scope struct {
	Branches []Branch
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
		scope.Branches = append(scope.Branches, Branch{
			labels.MustNewMatcher(labels.MatchEqual, "cluster", inventory.ManagementCluster),
			labels.MustNewMatcher(labels.MatchRegexp, "namespace", regex),
		})
	}
	var clusters []string
	for _, cluster := range org.Clusters {
		if cluster == "" || cluster == inventory.ManagementCluster {
			continue
		}
		clusters = append(clusters, cluster)
	}
	if regex := RegexFor(clusters); regex != "" {
		scope.Branches = append(scope.Branches, Branch{
			labels.MustNewMatcher(labels.MatchRegexp, "cluster", regex),
		})
	}
	return scope
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
