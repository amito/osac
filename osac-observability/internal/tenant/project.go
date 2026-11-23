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
	"context"

	"github.com/osac-project/osac/osac-observability/internal/inventory"
)

// scopeFor resolves the caller's scope. A Tenant Admin sees the whole
// organization and never calls the membership service. A Tenant User sees only
// their projects plus the default project; a resolution failure narrows the
// caller to the default project and never fails the request.
func (h *Handler) scopeFor(ctx context.Context, caller *Caller, org *inventory.Organization) Scope {
	if caller.Admin {
		return OrganizationScope(org)
	}
	projects := []string{""}
	if h.memberships != nil {
		members, err := h.memberships.Projects(ctx, caller.Subject, caller.Token)
		if err != nil {
			// The caller keeps their default project. The failure is logged at
			// debug level so an outage is visible without failing queries.
			h.logger.Debug("project membership lookup failed; using the default project",
				"subject", caller.Subject, "error", err)
		} else {
			projects = append(projects, members...)
		}
	}
	return ProjectScope(projectResources(org, projects))
}

// projectResources unions the resources of every named project. A project the
// inventory does not know contributes nothing, and an unknown organization
// yields no resources at all, which the scope renders as "admits nothing".
func projectResources(org *inventory.Organization, projects []string) *inventory.Project {
	if org == nil {
		return nil
	}
	merged := &inventory.Project{}
	for _, name := range projects {
		project := org.Projects[name]
		if project == nil {
			continue
		}
		merged.VMNamespaces = append(merged.VMNamespaces, project.VMNamespaces...)
		merged.VMNames = append(merged.VMNames, project.VMNames...)
		merged.TenantNamespaces = append(merged.TenantNamespaces, project.TenantNamespaces...)
		merged.VolumeNames = append(merged.VolumeNames, project.VolumeNames...)
		merged.HostedNamespaces = append(merged.HostedNamespaces, project.HostedNamespaces...)
		merged.ClusterNames = append(merged.ClusterNames, project.ClusterNames...)
	}
	merged.VMNamespaces = unionSorted(merged.VMNamespaces)
	merged.VMNames = unionSorted(merged.VMNames)
	merged.TenantNamespaces = unionSorted(merged.TenantNamespaces)
	merged.VolumeNames = unionSorted(merged.VolumeNames)
	merged.HostedNamespaces = unionSorted(merged.HostedNamespaces)
	merged.ClusterNames = unionSorted(merged.ClusterNames)
	return merged
}
