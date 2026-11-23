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

// blockingUpstream holds the first request until release is closed, so a test
// can prove a second request is refused before it reaches the backend.
type blockingUpstream struct {
	mu      sync.Mutex
	count   int
	block   bool
	entered chan struct{}
	release chan struct{}
}

func newBlockingUpstream() *blockingUpstream {
	return &blockingUpstream{
		block:   true,
		entered: make(chan struct{}, 8),
		release: make(chan struct{}),
	}
}

func (b *blockingUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.count++
	block := b.block
	b.mu.Unlock()
	if block {
		b.entered <- struct{}{}
		select {
		case <-b.release:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
}

func (b *blockingUpstream) requests() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count
}

func newLimitsHandler(t *testing.T, inventory Inventory, upstream http.Handler, limits *Limits) *Handler {
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
		Limits:    limits,
	})
	if err != nil {
		t.Fatalf("failed to build handler: %v", err)
	}
	return handler
}

func limitsInventory() Inventory {
	return availableInventory(map[string]*inventory.Organization{
		"acme":  {Namespaces: []string{"acme-vms"}, Clusters: []string{"acme-1"}},
		"other": {Namespaces: []string{"other-vms"}},
	})
}

func TestHandlerReturns429AtConcurrencyLimit(t *testing.T) {
	upstream := newBlockingUpstream()
	limits := newTestLimits(t, 1, 24*time.Hour, time.Minute)
	handler := newLimitsHandler(t, limitsInventory(), upstream, limits)

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- serve(handler, queryRequest(t, "/api/v1/query",
			url.Values{"query": {"up"}}, tenantToken(t, "acme")))
	}()
	select {
	case <-upstream.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first request never reached the backend")
	}

	second := serve(handler, queryRequest(t, "/api/v1/query",
		url.Values{"query": {"up"}}, tenantToken(t, "acme")))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", second.Code, second.Body.String())
	}
	if got := second.Header().Get("Retry-After"); got != "1" {
		t.Errorf("expected Retry-After: 1, got %q", got)
	}
	if upstream.requests() != 1 {
		t.Errorf("the refused request reached the backend: %d", upstream.requests())
	}

	upstream.mu.Lock()
	upstream.block = false
	upstream.mu.Unlock()
	close(upstream.release)
	first := <-firstDone
	if first.Code != http.StatusOK {
		t.Errorf("the first request failed: %d %s", first.Code, first.Body.String())
	}
	if got := limits.InFlight("acme"); got != 0 {
		t.Errorf("the slot was not released: %d", got)
	}
	third := serve(handler, queryRequest(t, "/api/v1/query",
		url.Values{"query": {"up"}}, tenantToken(t, "acme")))
	if third.Code != http.StatusOK {
		t.Errorf("a released slot must admit the next query: %d", third.Code)
	}
}

func TestHandler429AppliesToListingsToo(t *testing.T) {
	upstream := newBlockingUpstream()
	limits := newTestLimits(t, 1, 24*time.Hour, time.Minute)
	handler := newLimitsHandler(t, limitsInventory(), upstream, limits)
	go func() {
		serve(handler, queryRequest(t, "/api/v1/query",
			url.Values{"query": {"up"}}, tenantToken(t, "acme")))
	}()
	select {
	case <-upstream.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first request never reached the backend")
	}
	response := serve(handler, queryRequest(t, "/api/v1/labels", nil, tenantToken(t, "acme")))
	if response.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 for a listing, got %d", response.Code)
	}
	close(upstream.release)
}

func TestHandler429IsPerOrganization(t *testing.T) {
	upstream := newBlockingUpstream()
	limits := newTestLimits(t, 1, 24*time.Hour, time.Minute)
	handler := newLimitsHandler(t, limitsInventory(), upstream, limits)
	go func() {
		serve(handler, queryRequest(t, "/api/v1/query",
			url.Values{"query": {"up"}}, tenantToken(t, "acme")))
	}()
	select {
	case <-upstream.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first request never reached the backend")
	}
	upstream.mu.Lock()
	upstream.block = false
	upstream.mu.Unlock()
	response := serve(handler, queryRequest(t, "/api/v1/query",
		url.Values{"query": {"up"}}, tenantToken(t, "other")))
	if response.Code != http.StatusOK {
		t.Errorf("another organization must not be throttled: %d", response.Code)
	}
	close(upstream.release)
}

func TestHandlerOverRangeReturns422(t *testing.T) {
	upstream := &upstreamRecorder{}
	limits := newTestLimits(t, 10, 24*time.Hour, time.Minute)
	handler := newLimitsHandler(t, limitsInventory(), upstream, limits)
	response := serve(handler, queryRequest(t, "/api/v1/query_range", url.Values{
		"query": {"up"}, "start": {"0"}, "end": {"90000"}, "step": {"60"},
	}, tenantToken(t, "acme")))
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"errorType":"bad_data"`) {
		t.Errorf("unexpected body %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "24h") {
		t.Errorf("the error must name the limit: %s", response.Body.String())
	}
	if upstream.count() != 0 {
		t.Error("an over-range query must not reach the backend")
	}
}

func TestHandlerOverRangeDoesNotOverrideAnEmptyResult(t *testing.T) {
	upstream := &upstreamRecorder{}
	limits := newTestLimits(t, 10, 24*time.Hour, time.Minute)
	handler := newLimitsHandler(t, limitsInventory(), upstream, limits)
	// An unrewritable query answers 200 empty before the range check: no
	// backend work happens either way.
	response := serve(handler, queryRequest(t, "/api/v1/query_range", url.Values{
		"query": {"up[5m]"}, "start": {"0"}, "end": {"90000"}, "step": {"60"},
	}, tenantToken(t, "acme")))
	if response.Code != http.StatusOK || response.Body.String() != emptyMatrix {
		t.Fatalf("expected an empty 200, got %d %s", response.Code, response.Body.String())
	}
	if upstream.count() != 0 {
		t.Error("an unrewritable query must not reach the backend")
	}
}

func TestHandlerRejectsMalformedRangeBounds(t *testing.T) {
	upstream := &upstreamRecorder{}
	handler := newLimitsHandler(t, limitsInventory(), upstream, newTestLimits(t, 10, 24*time.Hour, time.Minute))
	response := serve(handler, queryRequest(t, "/api/v1/query_range", url.Values{
		"query": {"up"}, "start": {"0"}, "end": {"later"},
	}, tenantToken(t, "acme")))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", response.Code, response.Body.String())
	}
	if upstream.count() != 0 {
		t.Error("a malformed range must not reach the backend")
	}
}

func TestHandlerTimeoutReturns504(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond):
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
		case <-r.Context().Done():
		}
	})
	limits := newTestLimits(t, 10, 24*time.Hour, 50*time.Millisecond)
	handler := newLimitsHandler(t, limitsInventory(), slow, limits)
	response := serve(handler, queryRequest(t, "/api/v1/query",
		url.Values{"query": {"up"}}, tenantToken(t, "acme")))
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"errorType":"timeout"`) {
		t.Errorf("unexpected body %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "inventory") {
		t.Error("a slow query must not be reported as an inventory failure")
	}
}

func TestHandlerSlowRequestDoesNotLeakSlot(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond):
		case <-r.Context().Done():
		}
	})
	limits := newTestLimits(t, 1, 24*time.Hour, 50*time.Millisecond)
	handler := newLimitsHandler(t, limitsInventory(), slow, limits)
	response := serve(handler, queryRequest(t, "/api/v1/query",
		url.Values{"query": {"up"}}, tenantToken(t, "acme")))
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d", response.Code)
	}
	if got := limits.InFlight("acme"); got != 0 {
		t.Errorf("the timed-out query kept its slot: %d", got)
	}
	release, err := limits.Acquire("acme")
	if err != nil {
		t.Errorf("the full concurrency must be available again: %v", err)
	} else {
		release()
	}
}
