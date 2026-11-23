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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/osac-project/osac/osac-observability/internal/allowlist"
	"github.com/osac-project/osac/osac-observability/internal/inventory"
)

// ErrNoScope reports a caller whose scope admits nothing.
var ErrNoScope = errors.New("the caller's scope admits no series")

// ErrUnrewritable reports a query form this authorizer cannot narrow.
var ErrUnrewritable = errors.New("the query cannot be narrowed")

// inventoryUnavailable is the body of the identical 503 every caller receives
// while the inventory has no fresh list. It names nothing caller-specific.
const inventoryUnavailable = `{"status":"error","errorType":"unavailable",` +
	`"error":"the metrics inventory is unavailable"}`

const (
	emptyVector = `{"status":"success","data":{"resultType":"vector","result":[]}}`
	emptyMatrix = `{"status":"success","data":{"resultType":"matrix","result":[]}}`
	emptyList   = `{"status":"success","data":[]}`
)

// Inventory is the freshness-gated scope view.
type Inventory interface {
	Snapshot(now time.Time) (*inventory.Snapshot, bool)
}

// HandlerConfig configures the tenant authorizer.
type HandlerConfig struct {
	Identity  Config
	Inventory Inventory
	Upstream  *url.URL
	Client    *http.Client
	Now       func() time.Time
	Allow     *allowlist.Matcher
	Logger    *slog.Logger
}

// Handler serves the tenant front door: it authenticates, gates on inventory
// freshness, narrows the query to the caller's scope and forwards only the
// five documented endpoints.
type Handler struct {
	identity  Config
	inventory Inventory
	upstream  *url.URL
	client    *http.Client
	now       func() time.Time
	allow     *allowlist.Matcher
	logger    *slog.Logger
}

// NewHandler builds the tenant handler.
func NewHandler(config HandlerConfig) (*Handler, error) {
	if config.Upstream == nil {
		return nil, errors.New("upstream is required")
	}
	if config.Upstream.Scheme != "http" && config.Upstream.Scheme != "https" {
		return nil, fmt.Errorf("upstream %q must be an absolute http(s) URL", config.Upstream)
	}
	if config.Inventory == nil {
		return nil, errors.New("inventory is required")
	}
	handler := &Handler{
		identity:  config.Identity,
		inventory: config.Inventory,
		upstream:  config.Upstream,
		client:    config.Client,
		now:       config.Now,
		allow:     config.Allow,
		logger:    config.Logger,
	}
	if handler.client == nil {
		handler.client = &http.Client{Timeout: 2 * time.Minute}
	}
	if handler.now == nil {
		handler.now = time.Now
	}
	if handler.allow == nil {
		handler.allow = allowlist.Default()
	}
	if handler.logger == nil {
		handler.logger = slog.Default()
	}
	return handler, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/livez" || r.URL.Path == "/readyz" {
		// Readiness is deliberately not gated on inventory: a cold authorizer
		// must answer its own 503, not the router's.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	if !h.allow.Allowed(r.Method, r.URL.Path) {
		h.logger.Info("rejecting an unsupported endpoint", "path", r.URL.Path)
		writeNotImplemented(w, r.URL.Path)
		return
	}
	caller, err := Identify(h.identity, r)
	if err != nil {
		h.rejectIdentity(w, err)
		return
	}
	snapshot, available := h.inventory.Snapshot(h.now())
	if !available {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(inventoryUnavailable))
		return
	}
	scope := OrganizationScope(snapshot.Organizations[caller.Tenant])

	// A POST body is a form, not something to stream: parse it so the
	// rewritten parameters can be forwarded as a GET.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err = r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_data", "the request form is invalid")
		return
	}

	values := r.URL.Query()
	for key, list := range r.PostForm {
		values[key] = list
	}
	emptyBody := emptyVector
	switch r.URL.Path {
	case "/api/v1/query", "/api/v1/query_range":
		if r.URL.Path == "/api/v1/query_range" {
			emptyBody = emptyMatrix
		}
		query := values.Get("query")
		if query == "" {
			writeError(w, http.StatusBadRequest, "bad_data", "the query parameter is required")
			return
		}
		rewritten, err := h.rewriteQuery(scope, query)
		if err != nil {
			switch {
			case errors.Is(err, ErrNoScope), errors.Is(err, ErrUnrewritable):
				writeRaw(w, http.StatusOK, emptyBody)
			default:
				writeError(w, http.StatusBadRequest, "bad_data", err.Error())
			}
			return
		}
		values.Set("query", rewritten)
	default:
		selectors, err := h.rewriteMatch(values["match[]"], scope)
		if err != nil {
			switch {
			case errors.Is(err, ErrNoScope):
				writeRaw(w, http.StatusOK, emptyList)
			default:
				writeError(w, http.StatusBadRequest, "bad_data", err.Error())
			}
			return
		}
		values.Del("match[]")
		for _, selector := range selectors {
			values.Add("match[]", selector)
		}
	}
	h.forward(w, r, values)
}

func (h *Handler) rejectIdentity(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrMultipleTenants) {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="osac", error="invalid_token"`)
	writeError(w, http.StatusUnauthorized, "unauthorized", "a valid tenant login token is required")
}

// rewriteQuery narrows one PromQL expression to the caller's scope.
func (h *Handler) rewriteQuery(scope Scope, query string) (string, error) {
	expr, err := parseExpr(query)
	if err != nil {
		return "", fmt.Errorf("invalid query: %w", err)
	}
	rewritten, err := scope.Apply(expr)
	if err != nil {
		return "", err
	}
	return rewritten.String(), nil
}

// rewriteMatch expands every caller selector to the caller's scope. Absent or
// empty match[] means "every series", which is exactly the request that must
// never be forwarded: it is replaced by the scope selectors.
func (h *Handler) rewriteMatch(values []string, scope Scope) ([]string, error) {
	return expandMatch(values, scope)
}

// Apply narrows an expression to the scope. An empty scope or a form this
// version cannot narrow reports ErrNoScope or ErrUnrewritable so the handler
// answers with an empty result instead of forwarding the original query.
func (s Scope) Apply(expr parser.Expr) (parser.Expr, error) {
	selector, ok := expr.(*parser.VectorSelector)
	if !ok {
		return nil, ErrUnrewritable
	}
	return s.applySelector(selector)
}

func (s Scope) applySelector(selector *parser.VectorSelector) (parser.Expr, error) {
	if len(s.Branches) == 0 {
		return nil, ErrNoScope
	}
	if len(s.Branches) == 1 {
		selector.LabelMatchers = append(selector.LabelMatchers, s.Branches[0]...)
		return selector, nil
	}
	expressions := make([]parser.Expr, 0, len(s.Branches))
	for _, branch := range s.Branches {
		clone, err := cloneExpr(selector)
		if err != nil {
			return nil, ErrUnrewritable
		}
		clonedSelector, ok := clone.(*parser.VectorSelector)
		if !ok {
			return nil, ErrUnrewritable
		}
		clonedSelector.LabelMatchers = append(clonedSelector.LabelMatchers, branch...)
		expressions = append(expressions, clonedSelector)
	}
	return orExpr(expressions), nil
}

// expandMatch returns one selector per caller selector and scope branch. A
// bare selector (no name and no __name__ matcher) is scoped as-is, so the
// listing returns only the caller's series of every family.
func expandMatch(values []string, scope Scope) ([]string, error) {
	if len(scope.Branches) == 0 {
		return nil, ErrNoScope
	}
	if len(values) == 0 {
		values = []string{""}
	}
	var result []string
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			// An unconstrained listing: the scope itself is the selector.
			for _, branch := range scope.Branches {
				result = append(result, (&parser.VectorSelector{
					LabelMatchers: append([]*labels.Matcher(nil), branch...),
				}).String())
			}
			continue
		}
		expr, err := parseExpr(value)
		if err != nil {
			return nil, fmt.Errorf("invalid match[] selector: %w", err)
		}
		selector, ok := expr.(*parser.VectorSelector)
		if !ok {
			return nil, fmt.Errorf("match[] must be a series selector")
		}
		for _, branch := range scope.Branches {
			clone, err := cloneExpr(selector)
			if err != nil {
				return nil, ErrUnrewritable
			}
			clonedSelector, ok := clone.(*parser.VectorSelector)
			if !ok {
				return nil, ErrUnrewritable
			}
			clonedSelector.LabelMatchers = append(clonedSelector.LabelMatchers, branch...)
			result = append(result, clonedSelector.String())
		}
	}
	return result, nil
}

func (h *Handler) forward(w http.ResponseWriter, r *http.Request, values url.Values) {
	target := *h.upstream
	target.Path = r.URL.Path
	target.RawQuery = values.Encode()
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_gateway", "the query backend is unavailable")
		return
	}
	if accept := r.Header.Get("Accept"); accept != "" {
		request.Header.Set("Accept", accept)
	}
	response, err := h.client.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeError(w, http.StatusGatewayTimeout, "timeout", "the query exceeded its time limit")
			return
		}
		h.logger.Warn("query backend failed", "error", err)
		writeError(w, http.StatusBadGateway, "bad_gateway", "the query backend is unavailable")
		return
	}
	defer func() {
		_ = response.Body.Close()
	}()
	contentType := response.Header.Get("Content-Type")
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

type errorBody struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, errorType, message string) {
	payload, err := json.Marshal(errorBody{Status: "error", ErrorType: errorType, Error: message})
	if err != nil {
		http.Error(w, http.StatusText(status), status)
		return
	}
	writeRaw(w, status, string(payload))
}

func writeNotImplemented(w http.ResponseWriter, path string) {
	writeError(w, http.StatusNotImplemented, "not_implemented",
		fmt.Sprintf("the endpoint %s is not supported by this proxy", path))
}

func writeRaw(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
