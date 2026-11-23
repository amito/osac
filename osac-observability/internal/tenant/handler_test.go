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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osac-project/osac/osac-observability/internal/inventory"
)

type fakeInventory struct {
	snapshot *inventory.Snapshot
	ok       bool
}

func (f *fakeInventory) Snapshot(time.Time) (*inventory.Snapshot, bool) {
	return f.snapshot, f.ok
}

func availableInventory(orgs map[string]*inventory.Organization) *fakeInventory {
	return &fakeInventory{snapshot: &inventory.Snapshot{Organizations: orgs}, ok: true}
}

type upstreamRecorder struct {
	mu       sync.Mutex
	requests []*url.URL
	body     string
	status   int
	delay    time.Duration
}

func (u *upstreamRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.requests = append(u.requests, r.URL)
	body, status, delay := u.body, u.status, u.delay
	u.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if status == 0 {
		status = http.StatusOK
	}
	if body == "" {
		body = `{"status":"success","data":[]}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (u *upstreamRecorder) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

func (u *upstreamRecorder) lastQuery() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.requests) == 0 {
		return ""
	}
	return u.requests[len(u.requests)-1].Query().Get("query")
}

func (u *upstreamRecorder) lastValues() url.Values {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.requests) == 0 {
		return nil
	}
	return u.requests[len(u.requests)-1].Query()
}

func newTestHandler(t *testing.T, inventory Inventory, upstream *upstreamRecorder) *Handler {
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
		Inventory: inventory,
		Upstream:  target,
		Client:    server.Client(),
	})
	if err != nil {
		t.Fatalf("failed to build handler: %v", err)
	}
	return handler
}

func tenantToken(t *testing.T, tenant string) string {
	return token(t, map[string]any{"iss": testIssuer, "sub": "user-1", "organization": tenant})
}

func queryRequest(t *testing.T, path string, params url.Values, bearer string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "http://authorizer"+path+"?"+params.Encode(), nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	return request
}

func serve(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestHandlerRejectsUnauthenticated(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newTestHandler(t, availableInventory(nil), upstream)
	response := serve(handler, queryRequest(t, "/api/v1/query", url.Values{"query": {"up"}}, ""))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", response.Code)
	}
	if response.Header().Get("WWW-Authenticate") == "" {
		t.Error("expected a WWW-Authenticate header")
	}
	if upstream.count() != 0 {
		t.Error("an unauthenticated request must not reach the backend")
	}
}

func TestHandlerRejectsMultipleTenants(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newTestHandler(t, availableInventory(nil), upstream)
	bearer := token(t, map[string]any{
		"iss": testIssuer, "sub": "user-1", "organization": []any{"acme", "globex"},
	})
	response := serve(handler, queryRequest(t, "/api/v1/query", url.Values{"query": {"up"}}, bearer))
	if response.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", response.Code)
	}
	if upstream.count() != 0 {
		t.Error("a multi-tenant request must not reach the backend")
	}
}

func TestHandlerRejectsAdminClientToken(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newTestHandler(t, availableInventory(nil), upstream)
	bearer := token(t, map[string]any{
		"iss": testIssuer, "sub": "user-1", "organization": "acme", "azp": "thanos-admin",
	})
	response := serve(handler, queryRequest(t, "/api/v1/query", url.Values{"query": {"up"}}, bearer))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", response.Code)
	}
}

func TestHandlerReturns503BeforeFirstList(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newTestHandler(t, &fakeInventory{}, upstream)
	response := serve(handler, queryRequest(t, "/api/v1/query",
		url.Values{"query": {"up"}}, tenantToken(t, "acme")))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", response.Code)
	}
	if response.Body.String() != inventoryUnavailable {
		t.Errorf("unexpected 503 body: %s", response.Body.String())
	}
	if upstream.count() != 0 {
		t.Error("a cold inventory must not forward anything")
	}
}

func TestHandler503BodyIdenticalForEveryCaller(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newTestHandler(t, &fakeInventory{}, upstream)
	requests := []*http.Request{
		queryRequest(t, "/api/v1/query", url.Values{"query": {"up"}}, tenantToken(t, "acme")),
		queryRequest(t, "/api/v1/query_range", url.Values{"query": {"up"}}, tenantToken(t, "globex")),
		queryRequest(t, "/api/v1/labels", nil, tenantToken(t, "acme")),
		queryRequest(t, "/api/v1/series", url.Values{"match[]": {"up"}}, tenantToken(t, "globex")),
	}
	for _, request := range requests {
		response := serve(handler, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: expected 503, got %d", request.URL.Path, response.Code)
		}
		if response.Body.String() != inventoryUnavailable {
			t.Fatalf("%s: unexpected body %s", request.URL.Path, response.Body.String())
		}
	}
}

func TestHandlerReturns501ForUnlistedEndpoint(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newTestHandler(t, availableInventory(nil), upstream)
	for _, path := range []string{"/api/v1/status", "/api/v1/targets", "/api/v1/query_exemplars"} {
		response := serve(handler, queryRequest(t, path, nil, tenantToken(t, "acme")))
		if response.Code != http.StatusNotImplemented {
			t.Fatalf("%s: expected 501, got %d", path, response.Code)
		}
		if !strings.Contains(response.Body.String(), "not supported by this proxy") {
			t.Errorf("%s: unexpected body %s", path, response.Body.String())
		}
	}
	if upstream.count() != 0 {
		t.Error("an unlisted endpoint must not reach the backend")
	}
}

func TestHandlerRejectsMalformedPromQL(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newTestHandler(t, availableInventory(nil), upstream)
	response := serve(handler, queryRequest(t, "/api/v1/query",
		url.Values{"query": {"sum("}}, tenantToken(t, "acme")))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", response.Code)
	}
	if upstream.count() != 0 {
		t.Error("a malformed query must not reach the backend")
	}
}

func TestHandlerRewritesOrganizationScope(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newTestHandler(t, availableInventory(map[string]*inventory.Organization{
		"acme": {Namespaces: []string{"acme-vms"}, Clusters: []string{"acme-1"}},
	}), upstream)
	response := serve(handler, queryRequest(t, "/api/v1/query",
		url.Values{"query": {"kubevirt_vmi_cpu_usage_seconds_total"}}, tenantToken(t, "acme")))
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	want := `(kubevirt_vmi_cpu_usage_seconds_total{cluster="management",namespace=~"acme-vms"}` +
		` or kubevirt_vmi_cpu_usage_seconds_total{cluster=~"acme-1"})`
	if got := upstream.lastQuery(); got != want {
		t.Errorf("want %s\ngot  %s", want, got)
	}
}

func TestHandlerEmptyOrganizationReturnsEmptyResult(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newTestHandler(t, availableInventory(nil), upstream)
	response := serve(handler, queryRequest(t, "/api/v1/query",
		url.Values{"query": {"up"}}, tenantToken(t, "acme")))
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	if response.Body.String() != emptyVector {
		t.Errorf("unexpected body %s", response.Body.String())
	}
	if upstream.count() != 0 {
		t.Error("an empty scope must not reach the backend")
	}
}

func TestHandlerUnsupportedQueryFormReturnsEmptyResult(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newTestHandler(t, availableInventory(map[string]*inventory.Organization{
		"acme": {Namespaces: []string{"acme-vms"}},
	}), upstream)
	response := serve(handler, queryRequest(t, "/api/v1/query",
		url.Values{"query": {"rate(up[5m])"}}, tenantToken(t, "acme")))
	if response.Code != http.StatusOK || response.Body.String() != emptyVector {
		t.Fatalf("expected an empty 200, got %d %s", response.Code, response.Body.String())
	}
	if upstream.count() != 0 {
		t.Error("an unrewritable query must not reach the backend")
	}
}

func TestHandlerPreservesOtherParameters(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newTestHandler(t, availableInventory(map[string]*inventory.Organization{
		"acme": {Namespaces: []string{"acme-vms"}},
	}), upstream)
	response := serve(handler, queryRequest(t, "/api/v1/query_range", url.Values{
		"query": {"up"}, "start": {"1"}, "end": {"2"}, "step": {"15s"}, "timeout": {"1m"},
	}, tenantToken(t, "acme")))
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	values := upstream.lastValues()
	if values.Get("start") != "1" || values.Get("end") != "2" ||
		values.Get("step") != "15s" || values.Get("timeout") != "1m" {
		t.Errorf("parameters were not preserved: %v", values)
	}
	if !strings.Contains(values.Get("query"), `namespace=~"acme-vms"`) {
		t.Errorf("the query was not scoped: %s", values.Get("query"))
	}
}

func TestHandlerPOSTFormIsAccepted(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newTestHandler(t, availableInventory(map[string]*inventory.Organization{
		"acme": {Namespaces: []string{"acme-vms"}},
	}), upstream)
	form := url.Values{"query": {"up"}, "start": {"1"}}
	request, err := http.NewRequest(http.MethodPost, "http://authorizer/api/v1/query_range",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Bearer "+tenantToken(t, "acme"))
	response := serve(handler, request)
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	if got := upstream.lastQuery(); !strings.Contains(got, `namespace=~"acme-vms"`) {
		t.Errorf("the POST query was not scoped: %s", got)
	}
	if upstream.lastValues().Get("start") != "1" {
		t.Error("the POST parameters were not preserved")
	}
}

func TestHandlerForwardsUpstreamResponseVerbatim(t *testing.T) {
	upstream := &upstreamRecorder{body: `{"status":"success","data":{"resultType":"vector","result":[]}}`}
	handler := newTestHandler(t, availableInventory(map[string]*inventory.Organization{
		"acme": {Namespaces: []string{"acme-vms"}},
	}), upstream)
	response := serve(handler, queryRequest(t, "/api/v1/query",
		url.Values{"query": {"up"}}, tenantToken(t, "acme")))
	if response.Body.String() != upstream.body {
		t.Errorf("expected the upstream body verbatim, got %s", response.Body.String())
	}
}

func TestHandlerReturns502WhenUpstreamFails(t *testing.T) {
	handler, err := NewHandler(HandlerConfig{
		Identity: Config{Issuer: testIssuer, AdminClientID: "thanos-admin", AdminRole: "tenant-admin"},
		Inventory: availableInventory(map[string]*inventory.Organization{
			"acme": {Namespaces: []string{"acme-vms"}},
		}),
		Upstream: &url.URL{Scheme: "http", Host: "127.0.0.1:1"},
		Client:   &http.Client{Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("failed to build handler: %v", err)
	}
	response := serve(handler, queryRequest(t, "/api/v1/query",
		url.Values{"query": {"up"}}, tenantToken(t, "acme")))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", response.Code)
	}
}

func TestHandlerScopesAbsentMatch(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newTestHandler(t, availableInventory(map[string]*inventory.Organization{
		"acme": {Namespaces: []string{"acme-vms"}, Clusters: []string{"acme-1"}},
	}), upstream)
	for _, path := range []string{"/api/v1/labels", "/api/v1/label/__name__/values", "/api/v1/series"} {
		response := serve(handler, queryRequest(t, path, nil, tenantToken(t, "acme")))
		if response.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", path, response.Code)
		}
		values := upstream.lastValues()["match[]"]
		if len(values) != 2 {
			t.Fatalf("%s: expected two scope selectors, got %v", path, values)
		}
		if values[0] != `{cluster="management",namespace=~"acme-vms"}` &&
			values[1] != `{cluster="management",namespace=~"acme-vms"}` {
			t.Errorf("%s: the management branch is missing: %v", path, values)
		}
		if values[0] != `{cluster=~"acme-1"}` && values[1] != `{cluster=~"acme-1"}` {
			t.Errorf("%s: the cluster branch is missing: %v", path, values)
		}
	}
}

func TestHandlerScopesCallerMatch(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newTestHandler(t, availableInventory(map[string]*inventory.Organization{
		"acme": {Namespaces: []string{"acme-vms"}, Clusters: []string{"acme-1"}},
	}), upstream)
	response := serve(handler, queryRequest(t, "/api/v1/series",
		url.Values{"match[]": {`up{job="x"}`}}, tenantToken(t, "acme")))
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	values := upstream.lastValues()["match[]"]
	if len(values) != 2 {
		t.Fatalf("expected two selectors, got %v", values)
	}
	for _, value := range values {
		if !strings.Contains(value, `job="x"`) {
			t.Errorf("a caller matcher was dropped: %s", value)
		}
	}
}

func TestHandlerRejectsBadMatchSelector(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newTestHandler(t, availableInventory(map[string]*inventory.Organization{
		"acme": {Namespaces: []string{"acme-vms"}},
	}), upstream)
	response := serve(handler, queryRequest(t, "/api/v1/series",
		url.Values{"match[]": {"up{"}}, tenantToken(t, "acme")))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", response.Code)
	}
	if upstream.count() != 0 {
		t.Error("an invalid selector must not reach the backend")
	}
}

func TestHandlerProbesAreUngated(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newTestHandler(t, &fakeInventory{}, upstream)
	for _, path := range []string{"/livez", "/readyz"} {
		response := serve(handler, queryRequest(t, path, nil, ""))
		if response.Code != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", path, response.Code)
		}
	}
}
