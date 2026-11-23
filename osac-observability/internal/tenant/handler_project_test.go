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
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osac-project/osac/osac-observability/internal/inventory"
	"github.com/osac-project/osac/osac-observability/internal/membership"
)

// membershipStub records every resolution and answers from a fixed table.
type membershipStub struct {
	mu       sync.Mutex
	calls    int
	subjects []string
	tokens   []string
	projects []string
	err      error
}

func (s *membershipStub) List(_ context.Context, subject, token string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.subjects = append(s.subjects, subject)
	s.tokens = append(s.tokens, token)
	if s.err != nil {
		return nil, s.err
	}
	return append([]string(nil), s.projects...), nil
}

func (s *membershipStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *membershipStub) lastToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tokens) == 0 {
		return ""
	}
	return s.tokens[len(s.tokens)-1]
}

func (s *membershipStub) lastSubject() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.subjects) == 0 {
		return ""
	}
	return s.subjects[len(s.subjects)-1]
}

// manualClock advances only when the test says so.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func projectInventory() *fakeInventory {
	return availableInventory(map[string]*inventory.Organization{
		"acme": {
			Namespaces: []string{"acme-tenant"},
			Clusters:   []string{"acme-c1"},
			Projects: map[string]*inventory.Project{
				"": {HostedNamespaces: []string{"acme-default-cp"}},
				"team-a": {
					VMNamespaces:     []string{"team-a-vms"},
					VMNames:          []string{"vm-a"},
					HostedNamespaces: []string{"team-a-cp"},
					ClusterNames:     []string{"team-a-c1"},
				},
				"team-b": {
					VMNamespaces:     []string{"team-b-vms"},
					VMNames:          []string{"vm-b"},
					HostedNamespaces: []string{"team-b-cp"},
					ClusterNames:     []string{"team-b-c1"},
				},
			},
		},
	})
}

func newMembershipTestHandler(t *testing.T, source Inventory, upstream *upstreamRecorder,
	memberships *membership.Cache) *Handler {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("failed to parse upstream: %v", err)
	}
	handler, err := NewHandler(HandlerConfig{
		Identity: Config{
			Issuer:        testIssuer,
			AdminClientID: "thanos-admin",
			AdminRole:     "tenant-admin",
		},
		Inventory:   source,
		Upstream:    target,
		Client:      server.Client(),
		Memberships: memberships,
	})
	if err != nil {
		t.Fatalf("failed to build handler: %v", err)
	}
	return handler
}

func userToken(t *testing.T, tenant, subject string) string {
	t.Helper()
	return token(t, map[string]any{
		"iss": testIssuer, "sub": subject, "organization": tenant,
	})
}

func serveQuery(t *testing.T, handler *Handler, bearer, prom string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(handler, queryRequest(t, "/api/v1/query", url.Values{"query": {prom}}, bearer))
}

func TestHandlerAdminUsesOrganizationScope(t *testing.T) {
	upstream := &upstreamRecorder{}
	lister := &membershipStub{}
	handler := newMembershipTestHandler(t, projectInventory(), upstream,
		&membership.Cache{Lister: lister.List})
	response := serveQuery(t, handler, adminToken(t, "acme"), "up")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	want := `(up{cluster="management",namespace=~"acme-tenant"} or up{cluster=~"acme-c1"})`
	if got := upstream.lastQuery(); got != want {
		t.Errorf("want %s\ngot  %s", want, got)
	}
}

func TestHandlerAdminScopeDoesNotCallMembership(t *testing.T) {
	upstream := &upstreamRecorder{}
	lister := &membershipStub{}
	handler := newMembershipTestHandler(t, projectInventory(), upstream,
		&membership.Cache{Lister: lister.List})
	if response := serveQuery(t, handler, adminToken(t, "acme"), "up"); response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	if lister.callCount() != 0 {
		t.Error("an admin request must never call the membership service")
	}
}

func TestHandlerUserUsesProjectScope(t *testing.T) {
	upstream := &upstreamRecorder{}
	lister := &membershipStub{projects: []string{"team-a"}}
	handler := newMembershipTestHandler(t, projectInventory(), upstream,
		&membership.Cache{Lister: lister.List})
	response := serveQuery(t, handler, userToken(t, "acme", "alice"),
		"kubevirt_vmi_cpu_usage_seconds_total")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	metric := "kubevirt_vmi_cpu_usage_seconds_total"
	want := "(" + metric + `{cluster="management",name=~"vm-a",namespace=~"team-a-vms"} or ` +
		metric + `{cluster="management",namespace=~"acme-default-cp|team-a-cp"} or ` +
		metric + `{cluster=~"team-a-c1"})`
	if got := upstream.lastQuery(); got != want {
		t.Errorf("want %s\ngot  %s", want, got)
	}
	if lister.callCount() != 1 {
		t.Errorf("one membership resolution per request, got %d", lister.callCount())
	}
	if got := lister.lastSubject(); got != "alice" {
		t.Errorf("unexpected subject: %q", got)
	}
}

func TestHandlerForwardsMembershipToken(t *testing.T) {
	upstream := &upstreamRecorder{}
	lister := &membershipStub{projects: []string{"team-a"}}
	handler := newMembershipTestHandler(t, projectInventory(), upstream,
		&membership.Cache{Lister: lister.List})
	bearer := userToken(t, "acme", "alice")
	if response := serveQuery(t, handler, bearer, "up"); response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	if got := lister.lastToken(); got != bearer {
		t.Error("the caller's token must be forwarded unchanged")
	}
}

func TestHandlerMembershipFailureUsesDefaultProject(t *testing.T) {
	upstream := &upstreamRecorder{}
	lister := &membershipStub{err: errors.New("fulfillment is down")}
	handler := newMembershipTestHandler(t, projectInventory(), upstream,
		&membership.Cache{Lister: lister.List})
	response := serveQuery(t, handler, userToken(t, "acme", "alice"), "up")
	if response.Code != http.StatusOK {
		t.Fatalf("a resolution outage must not fail the request: %d", response.Code)
	}
	want := `up{cluster="management",namespace=~"acme-default-cp"}`
	if got := upstream.lastQuery(); got != want {
		t.Errorf("want the default project only, got %s", got)
	}
}

func TestHandlerProjectScopeEmptyResultForSiblingProject(t *testing.T) {
	upstream := &upstreamRecorder{}
	lister := &membershipStub{projects: []string{"team-b"}}
	handler := newMembershipTestHandler(t, projectInventory(), upstream,
		&membership.Cache{Lister: lister.List})
	response := serveQuery(t, handler, userToken(t, "acme", "alice"),
		"kubevirt_vmi_cpu_usage_seconds_total")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	query := upstream.lastQuery()
	if strings.Contains(query, "team-a") {
		t.Errorf("a sibling project leaked into the predicate: %s", query)
	}
	if !strings.Contains(query, "team-b-vms") || !strings.Contains(query, `name=~"vm-b"`) {
		t.Errorf("the caller's own project is missing: %s", query)
	}
}

func TestHandlerMembershipCacheAcrossRequests(t *testing.T) {
	upstream := &upstreamRecorder{}
	lister := &membershipStub{projects: []string{"team-a"}}
	clock := newManualClock()
	handler := newMembershipTestHandler(t, projectInventory(), upstream,
		&membership.Cache{TTL: 30 * time.Second, Now: clock.Now, Lister: lister.List})
	bearer := userToken(t, "acme", "alice")
	for i := 0; i < 2; i++ {
		clock.Advance(10 * time.Second)
		if response := serveQuery(t, handler, bearer, "up"); response.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", response.Code)
		}
	}
	if lister.callCount() != 1 {
		t.Errorf("two requests inside the window must share one resolution: %d",
			lister.callCount())
	}
}

func TestHandlerMembershipCacheExpiry(t *testing.T) {
	upstream := &upstreamRecorder{}
	lister := &membershipStub{projects: []string{"team-a"}}
	clock := newManualClock()
	handler := newMembershipTestHandler(t, projectInventory(), upstream,
		&membership.Cache{TTL: 30 * time.Second, Now: clock.Now, Lister: lister.List})
	bearer := userToken(t, "acme", "alice")
	if response := serveQuery(t, handler, bearer, "up"); response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	clock.Advance(31 * time.Second)
	if response := serveQuery(t, handler, bearer, "up"); response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	if lister.callCount() != 2 {
		t.Errorf("the window must expire: %d calls", lister.callCount())
	}
}

func TestHandlerProjectScopeUnrewritableReturnsEmpty(t *testing.T) {
	upstream := &upstreamRecorder{}
	lister := &membershipStub{projects: []string{"team-a"}}
	handler := newMembershipTestHandler(t, projectInventory(), upstream,
		&membership.Cache{Lister: lister.List})
	response := serveQuery(t, handler, userToken(t, "acme", "alice"),
		"kube_persistentvolume_capacity_bytes")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	if response.Body.String() != emptyVector {
		t.Errorf("unexpected body: %s", response.Body.String())
	}
	if upstream.count() != 0 {
		t.Error("an unmatchable query must not reach the backend")
	}
}

func TestHandlerUserWithoutMembershipClientUsesDefaultProject(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newMembershipTestHandler(t, projectInventory(), upstream, nil)
	response := serveQuery(t, handler, userToken(t, "acme", "alice"), "up")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	want := `up{cluster="management",namespace=~"acme-default-cp"}`
	if got := upstream.lastQuery(); got != want {
		t.Errorf("want the default project only, got %s", got)
	}
}

// admit applies Prometheus matcher semantics: a series is admitted when any
// branch accepts it.
func admit(branches []Branch, series map[string]string) bool {
	for _, branch := range branches {
		admitted := true
		for _, matcher := range branch {
			if !matcher.Matches(series[matcher.Name]) {
				admitted = false
				break
			}
		}
		if admitted {
			return true
		}
	}
	return false
}

func TestAdmitProjectBranches(t *testing.T) {
	org := projectInventory().snapshot.Organizations["acme"]
	project := projectResources(org, []string{"", "team-a"})
	branches := ProjectScope(project).BranchesFor("kubevirt_vmi_info", true)
	tests := []struct {
		name   string
		series map[string]string
		want   bool
	}{
		{
			name:   "the family matcher admits the project's VM",
			series: map[string]string{"cluster": "management", "namespace": "team-a-vms", "name": "vm-a"},
			want:   true,
		},
		{
			name:   "a sibling project's VM is not admitted",
			series: map[string]string{"cluster": "management", "namespace": "team-b-vms", "name": "vm-b"},
			want:   false,
		},
		{
			name:   "a hosted namespace is admitted",
			series: map[string]string{"cluster": "management", "namespace": "team-a-cp"},
			want:   true,
		},
		{
			name:   "a tenant cluster is admitted",
			series: map[string]string{"cluster": "team-a-c1", "namespace": "anything"},
			want:   true,
		},
		{
			name:   "the default project is admitted",
			series: map[string]string{"cluster": "management", "namespace": "acme-default-cp"},
			want:   true,
		},
		{
			name:   "a resource without a project annotation is invisible",
			series: map[string]string{"cluster": "management", "namespace": "acme-tenant"},
			want:   false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := admit(branches, test.series); got != test.want {
				t.Errorf("want %v, got %v", test.want, got)
			}
		})
	}
}

func TestHandlerProjectAndOrgPredicatesAreIdentityCases(t *testing.T) {
	// One project owns everything the organization's namespaces and clusters
	// hold, so both predicates must admit exactly the same real series.
	organization := &inventory.Organization{
		Namespaces: []string{"vms", "cp"},
		Clusters:   []string{"c-1"},
		Projects: map[string]*inventory.Project{
			"p1": {
				VMNamespaces:     []string{"vms"},
				VMNames:          []string{"vm-1"},
				HostedNamespaces: []string{"cp"},
				ClusterNames:     []string{"c-1"},
			},
		},
	}
	project := projectResources(organization, []string{"p1"})
	orgBranches := OrganizationScope(organization).BranchesFor("kubevirt_vmi_info", true)
	projectBranches := ProjectScope(project).BranchesFor("kubevirt_vmi_info", true)
	series := []map[string]string{
		{"cluster": "management", "namespace": "vms", "name": "vm-1"},
		{"cluster": "management", "namespace": "cp"},
		{"cluster": "c-1", "namespace": "anything"},
		{"cluster": "management", "namespace": "other"},
	}
	want := []bool{true, true, true, false}
	for index, candidate := range series {
		org := admit(orgBranches, candidate)
		project := admit(projectBranches, candidate)
		if org != project {
			t.Errorf("series %v: organization admits %v, project admits %v", candidate, org, project)
		}
		if org != want[index] {
			t.Errorf("series %v: want %v, got %v", candidate, want[index], org)
		}
	}
}
