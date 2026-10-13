/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package servers

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osac-project/osac/fulfillment-service/internal/auth"
	"github.com/osac-project/osac/fulfillment-service/internal/billing"
	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
	"github.com/prometheus/client_golang/prometheus"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

const (
	catalogPricingCacheTTL                = 5 * time.Minute
	catalogPricingTimeout                 = 15 * time.Second
	catalogPricingRetryCount              = 3
	catalogPricingCircuitFailureThreshold = 5
	catalogPricingCircuitFailureWindow    = 30 * time.Second
	catalogPricingCircuitOpenDuration     = 30 * time.Second
	billingCoverageTimeout                = 30 * time.Second
)

type catalogPricingEnrichmentDisabledKey struct{}

func withoutCatalogPricing(ctx context.Context) context.Context {
	return context.WithValue(ctx, catalogPricingEnrichmentDisabledKey{}, true)
}

func catalogPricingEnrichmentDisabled(ctx context.Context) bool {
	return ctx.Value(catalogPricingEnrichmentDisabledKey{}) == true
}

// CatalogPricingRequest describes one billable catalog or selected configuration.
type CatalogPricingRequest = billing.CatalogPricingRequest

type catalogPricingAccountStore interface {
	GetAssignedAccount(context.Context, string) (billing.AssignedAccount, error)
}

// CatalogPricingResolver applies provider rates to catalog default configurations.
type CatalogPricingResolver struct {
	providerKey   string
	provider      billing.RateCardProvider
	accountStatus billing.AccountStatusProvider
	accounts      catalogPricingAccountStore
	now           func() time.Time

	mu       sync.Mutex
	cache    map[catalogRateCardKey]catalogRateCardEntry
	inFlight map[catalogRateCardKey]*catalogRateCardLoad
	circuit  catalogPricingCircuitBreaker
	metrics  *catalogPricingMetrics
}

type catalogRateCardKey struct {
	providerKey string
	account     string
	currency    string
	service     string
}

type catalogRateCardEntry struct {
	items     []billing.RateCardItem
	fetchedAt time.Time
}

type catalogRateCardLoad struct {
	done  chan struct{}
	items []billing.RateCardItem
	err   error
}

// NewCatalogPricingResolver creates a resolver for the configured billing provider.
func NewCatalogPricingResolver(
	providerKey string,
	provider billing.RateCardProvider,
	accounts catalogPricingAccountStore,
	registerers ...prometheus.Registerer,
) *CatalogPricingResolver {
	accountStatus, _ := provider.(billing.AccountStatusProvider)
	resolver := &CatalogPricingResolver{
		providerKey:   providerKey,
		provider:      provider,
		accountStatus: accountStatus,
		accounts:      accounts,
		now:           time.Now,
		cache:         make(map[catalogRateCardKey]catalogRateCardEntry),
		inFlight:      make(map[catalogRateCardKey]*catalogRateCardLoad),
	}
	if len(registerers) > 0 && registerers[0] != nil {
		resolver.metrics = newCatalogPricingMetrics(registerers[0])
	}
	return resolver
}

// Resolve returns catalog prices for one tenant's linked account. Failed or
// uncovered lookups produce UNAVAILABLE results so catalog browsing remains
// available.
func (r *CatalogPricingResolver) Resolve(
	ctx context.Context,
	tenantID string,
	requests []CatalogPricingRequest,
) []*publicv1.CatalogItemPricing {
	if len(requests) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, catalogPricingTimeout)
	defer cancel()

	results := make([]*publicv1.CatalogItemPricing, len(requests))
	if r != nil && r.metrics != nil {
		defer func() { r.metrics.recordResults(r.providerKey, results) }()
	}
	for i, request := range requests {
		results[i] = unavailableCatalogPricing("", request.PriceMayVary)
	}
	if r == nil || r.accounts == nil || r.providerKey == "" || tenantID == "" {
		return results
	}

	account, err := r.accounts.GetAssignedAccount(ctx, tenantID)
	if err != nil {
		return results
	}
	account.CurrencyCode = strings.ToUpper(strings.TrimSpace(account.CurrencyCode))
	for i, request := range requests {
		results[i] = unavailableCatalogPricing(account.CurrencyCode, request.PriceMayVary)
	}
	if r.provider == nil || !assignmentIsConnected(account.ConnectionState) || account.ProviderKey != r.providerKey || account.CurrencyCode == "" || account.ProviderAccountReference == "" {
		return results
	}

	indicesByService := make(map[string][]int)
	for i, request := range requests {
		if request.Supported && request.Service != "" {
			indicesByService[request.Service] = append(indicesByService[request.Service], i)
		}
	}
	for service, indices := range indicesByService {
		rateItems, stale := r.getRateCardItems(ctx, account, service)
		if rateItems == nil {
			continue
		}
		for _, index := range indices {
			request := requests[index]
			matched, matchErr := billing.ResolveRateCardItem(rateItems, service, account.CurrencyCode, request.Dimensions)
			if matchErr != nil {
				continue
			}
			status := publicv1.CatalogPricingStatus_CATALOG_PRICING_STATUS_CURRENT
			if stale {
				status = publicv1.CatalogPricingStatus_CATALOG_PRICING_STATUS_STALE
			}
			results[index] = publicv1.CatalogItemPricing_builder{
				Status:       status,
				CurrencyCode: &account.CurrencyCode,
				PriceMayVary: request.PriceMayVary,
				Components: []*publicv1.CatalogPriceComponent{publicv1.CatalogPriceComponent_builder{
					Amount:      matched.Amount,
					Unit:        string(matched.Unit),
					Description: request.Description,
				}.Build()},
			}.Build()
		}
	}
	return results
}

// CheckCoverage requires a fresh rate-card lookup for every billable service in
// the requested configuration. Cached catalog prices are deliberately ignored:
// a retained amount can help browsing, but cannot authorize a new launch.
func (r *CatalogPricingResolver) CheckCoverage(
	ctx context.Context,
	tenantID string,
	requests []CatalogPricingRequest,
) error {
	if len(requests) == 0 || r == nil {
		return nil
	}
	if tenantID == "" {
		return grpcstatus.Error(grpccodes.InvalidArgument, "tenant is required for billing coverage")
	}
	if r.provider == nil || r.accounts == nil || r.providerKey == "" {
		return grpcstatus.Error(grpccodes.Unavailable, "BILLING_COVERAGE_UNAVAILABLE")
	}
	if r.accountStatus == nil {
		return grpcstatus.Error(grpccodes.Unavailable, "BILLING_ACCOUNT_STATUS_UNAVAILABLE")
	}

	ctx, cancel := context.WithTimeout(ctx, billingCoverageTimeout)
	defer cancel()
	account, err := r.accounts.GetAssignedAccount(ctx, tenantID)
	if errors.Is(err, billing.ErrTenantBillingAssignmentNotFound) {
		return grpcstatus.Error(grpccodes.FailedPrecondition, "BILLING_ACCOUNT_UNAVAILABLE")
	}
	if err != nil {
		if ctx.Err() != nil {
			return grpcstatus.Error(grpccodes.DeadlineExceeded, "BILLING_COVERAGE_TIMEOUT")
		}
		return grpcstatus.Error(grpccodes.Internal, "BILLING_ACCOUNT_LOOKUP_FAILED")
	}
	account.CurrencyCode = strings.ToUpper(strings.TrimSpace(account.CurrencyCode))
	if account.ProviderKey != r.providerKey || !assignmentIsConnected(account.ConnectionState) || account.ProviderAccountReference == "" || account.CurrencyCode == "" {
		return grpcstatus.Error(grpccodes.FailedPrecondition, "BILLING_ACCOUNT_UNAVAILABLE")
	}
	providerAccount, err := r.accountStatus.GetAccount(ctx, account.ProviderAccountReference)
	if err != nil {
		if ctx.Err() != nil {
			return grpcstatus.Error(grpccodes.DeadlineExceeded, "BILLING_COVERAGE_TIMEOUT")
		}
		return grpcstatus.Error(grpccodes.Unavailable, "BILLING_ACCOUNT_STATUS_UNAVAILABLE")
	}
	if providerAccount.Reference != account.ProviderAccountReference || providerAccount.State != billing.AccountStateActive {
		return grpcstatus.Error(grpccodes.FailedPrecondition, "BILLING_ACCOUNT_UNAVAILABLE")
	}

	byService := make(map[string][]CatalogPricingRequest)
	for _, request := range requests {
		if !request.Supported || request.Service == "" {
			return grpcstatus.Error(grpccodes.FailedPrecondition, "BILLING_CONFIGURATION_UNSUPPORTED")
		}
		byService[request.Service] = append(byService[request.Service], request)
	}
	services := make([]string, 0, len(byService))
	for service := range byService {
		services = append(services, service)
	}
	sort.Strings(services)
	for _, service := range services {
		rateItems, err := r.getFreshRateCardItems(ctx, account, service)
		if err != nil {
			if ctx.Err() != nil {
				return grpcstatus.Error(grpccodes.DeadlineExceeded, "BILLING_COVERAGE_TIMEOUT")
			}
			return grpcstatus.Error(grpccodes.Unavailable, "BILLING_COVERAGE_UNAVAILABLE")
		}
		for _, request := range byService[service] {
			_, err := billing.ResolveRateCardItem(rateItems, service, account.CurrencyCode, request.Dimensions)
			switch {
			case err == nil:
				continue
			case errors.Is(err, billing.ErrRateCoverageGap):
				return grpcstatus.Error(grpccodes.FailedPrecondition, "BILLING_COVERAGE_GAP")
			case errors.Is(err, billing.ErrAmbiguousRateCard):
				return grpcstatus.Error(grpccodes.FailedPrecondition, "BILLING_RATE_AMBIGUOUS")
			default:
				return grpcstatus.Error(grpccodes.Unavailable, "BILLING_COVERAGE_UNAVAILABLE")
			}
		}
	}
	return nil
}

// checkCatalogItemBillingCoverage requires current coverage when a tenant-owned
// catalog item is offered for provisioning. Shared and system catalog items have
// no single tenant account to check; their launches are checked for each tenant.
func checkCatalogItemBillingCoverage(
	ctx context.Context,
	resolver *CatalogPricingResolver,
	tenantID string,
	published bool,
	requests []CatalogPricingRequest,
) error {
	if !catalogItemNeedsBillingCoverage(resolver, tenantID, published) {
		return nil
	}
	return resolver.CheckCoverage(ctx, tenantID, requests)
}

func catalogItemNeedsBillingCoverage(resolver *CatalogPricingResolver, tenantID string, published bool) bool {
	return published && resolver != nil && tenantID != auth.SharedTenant && tenantID != auth.SystemTenant
}

func (r *CatalogPricingResolver) getFreshRateCardItems(
	ctx context.Context,
	account billing.AssignedAccount,
	service string,
) ([]billing.RateCardItem, error) {
	allowed, probe := r.circuit.allow(r.now())
	if !allowed {
		if r.metrics != nil && r.metrics.lookupOutcomes != nil {
			r.metrics.lookupOutcomes.WithLabelValues(r.providerKey, "circuit_open").Inc()
		}
		return nil, errors.New("billing pricing circuit is open")
	}
	startedAt := time.Now()
	items, err := listRateCardsWithRetries(ctx, r.provider, billing.RateCardQuery{
		AccountReference: account.ProviderAccountReference,
		Service:          service,
		CurrencyCode:     account.CurrencyCode,
	})
	if r.metrics != nil && r.metrics.lookupOutcomes != nil {
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		r.metrics.lookupOutcomes.WithLabelValues(r.providerKey, outcome).Inc()
	}
	if r.metrics != nil && r.metrics.lookupDuration != nil {
		r.metrics.lookupDuration.WithLabelValues(r.providerKey).Observe(time.Since(startedAt).Seconds())
	}
	if err != nil {
		r.circuit.recordFailure(r.now(), probe)
		return nil, err
	}
	r.circuit.recordSuccess(probe)
	return items, nil
}

func (r *CatalogPricingResolver) getRateCardItems(
	ctx context.Context,
	account billing.AssignedAccount,
	service string,
) ([]billing.RateCardItem, bool) {
	key := catalogRateCardKey{
		providerKey: r.providerKey,
		account:     account.ProviderAccountReference,
		currency:    account.CurrencyCode,
		service:     service,
	}
	now := r.now()

	r.mu.Lock()
	entry, hasEntry := r.cache[key]
	if hasEntry && now.Sub(entry.fetchedAt) < catalogPricingCacheTTL {
		r.mu.Unlock()
		return entry.items, false
	}
	if loading, ok := r.inFlight[key]; ok {
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			if hasEntry {
				return entry.items, true
			}
			return nil, false
		case <-loading.done:
			if loading.err == nil {
				return loading.items, false
			}
			if hasEntry {
				return entry.items, true
			}
			return nil, false
		}
	}
	allowed, probe := r.circuit.allow(now)
	if !allowed {
		if r.metrics != nil && r.metrics.lookupOutcomes != nil {
			r.metrics.lookupOutcomes.WithLabelValues(r.providerKey, "circuit_open").Inc()
		}
		r.mu.Unlock()
		if hasEntry {
			return entry.items, true
		}
		return nil, false
	}
	loading := &catalogRateCardLoad{done: make(chan struct{})}
	r.inFlight[key] = loading
	r.mu.Unlock()

	query := billing.RateCardQuery{
		AccountReference: account.ProviderAccountReference,
		Service:          service,
		CurrencyCode:     account.CurrencyCode,
	}
	startedAt := time.Now()
	items, err := listRateCardsWithRetries(ctx, r.provider, query)
	if err == nil && items == nil {
		// A provider may represent an empty rate card with a nil slice. Keep that
		// distinct from nil returned after a failed lookup, which means unavailable.
		items = []billing.RateCardItem{}
	}
	if r.metrics != nil && r.metrics.lookupOutcomes != nil {
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		r.metrics.lookupOutcomes.WithLabelValues(r.providerKey, outcome).Inc()
	}
	if r.metrics != nil && r.metrics.lookupDuration != nil {
		r.metrics.lookupDuration.WithLabelValues(r.providerKey).Observe(time.Since(startedAt).Seconds())
	}
	if err != nil {
		r.circuit.recordFailure(r.now(), probe)
	} else {
		r.circuit.recordSuccess(probe)
	}

	r.mu.Lock()
	loading.items = items
	loading.err = err
	if err == nil {
		r.cache[key] = catalogRateCardEntry{items: items, fetchedAt: r.now()}
	}
	delete(r.inFlight, key)
	close(loading.done)
	if err == nil {
		r.mu.Unlock()
		return items, false
	}
	entry, hasEntry = r.cache[key]
	r.mu.Unlock()
	if hasEntry {
		return entry.items, true
	}
	return nil, false
}

func listRateCardsWithRetries(ctx context.Context, provider billing.RateCardProvider, query billing.RateCardQuery) ([]billing.RateCardItem, error) {
	var lastErr error
	for attempt := 0; attempt <= catalogPricingRetryCount; attempt++ {
		items, err := provider.ListRateCardItems(ctx, query)
		if err == nil {
			return items, nil
		}
		lastErr = err
		if attempt == catalogPricingRetryCount {
			break
		}
		timer := time.NewTimer(time.Duration(1<<attempt) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

type catalogPricingCircuitBreaker struct {
	mu            sync.Mutex
	failures      []time.Time
	openUntil     time.Time
	probeInFlight bool
}

func (b *catalogPricingCircuitBreaker) allow(now time.Time) (allowed, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true, false
	}
	if now.Before(b.openUntil) || b.probeInFlight {
		return false, false
	}
	b.probeInFlight = true
	return true, true
}

func (b *catalogPricingCircuitBreaker) recordSuccess(probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if probe {
		b.failures = nil
		b.openUntil = time.Time{}
		b.probeInFlight = false
	}
}

func (b *catalogPricingCircuitBreaker) recordFailure(now time.Time, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if probe {
		b.probeInFlight = false
		b.failures = nil
		b.openUntil = now.Add(catalogPricingCircuitOpenDuration)
		return
	}
	cutoff := now.Add(-catalogPricingCircuitFailureWindow)
	recent := b.failures[:0]
	for _, failure := range b.failures {
		if failure.After(cutoff) {
			recent = append(recent, failure)
		}
	}
	b.failures = append(recent, now)
	if len(b.failures) >= catalogPricingCircuitFailureThreshold {
		b.failures = nil
		b.openUntil = now.Add(catalogPricingCircuitOpenDuration)
	}
}

func unavailableCatalogPricing(currency string, priceMayVary bool) *publicv1.CatalogItemPricing {
	var currencyCode *string
	if currency != "" {
		currencyCode = &currency
	}
	return publicv1.CatalogItemPricing_builder{
		Status:       publicv1.CatalogPricingStatus_CATALOG_PRICING_STATUS_UNAVAILABLE,
		CurrencyCode: currencyCode,
		PriceMayVary: priceMayVary,
	}.Build()
}

func resolveCatalogPricing(
	ctx context.Context,
	tenancyLogic auth.TenancyLogic,
	resolver *CatalogPricingResolver,
	requests []CatalogPricingRequest,
) []*publicv1.CatalogItemPricing {
	if resolver == nil || catalogPricingEnrichmentDisabled(ctx) {
		return nil
	}
	tenantID, err := tenancyLogic.DetermineDefaultTenant(ctx)
	if err != nil || tenantID == "" {
		results := make([]*publicv1.CatalogItemPricing, len(requests))
		for i, request := range requests {
			results[i] = unavailableCatalogPricing("", request.PriceMayVary)
		}
		return results
	}
	return resolver.Resolve(ctx, tenantID, requests)
}

type catalogPricingMetrics struct {
	lookupOutcomes *prometheus.CounterVec
	lookupDuration *prometheus.HistogramVec
	results        *prometheus.CounterVec
}

func newCatalogPricingMetrics(registerer prometheus.Registerer) *catalogPricingMetrics {
	metrics := &catalogPricingMetrics{}
	lookupOutcomes := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "osac_billing_provider_pricing_lookups_total",
		Help: "Catalog pricing lookups by configured provider and outcome.",
	}, []string{"provider", "outcome"})
	lookupDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "osac_billing_provider_pricing_lookup_duration_seconds",
		Help:    "Time spent retrieving provider pricing for catalog reads.",
		Buckets: prometheus.DefBuckets,
	}, []string{"provider"})
	results := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "osac_billing_catalog_pricing_results_total",
		Help: "Catalog pricing results by configured provider and freshness state.",
	}, []string{"provider", "status"})
	metrics.lookupOutcomes = registerCatalogPricingCounter(registerer, lookupOutcomes)
	metrics.lookupDuration = registerCatalogPricingHistogram(registerer, lookupDuration)
	metrics.results = registerCatalogPricingCounter(registerer, results)
	return metrics
}

func registerCatalogPricingCounter(registerer prometheus.Registerer, collector *prometheus.CounterVec) *prometheus.CounterVec {
	if err := registerer.Register(collector); err != nil {
		var alreadyRegistered prometheus.AlreadyRegisteredError
		if errors.As(err, &alreadyRegistered) {
			if existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.CounterVec); ok {
				return existing
			}
		}
		return nil
	}
	return collector
}

func registerCatalogPricingHistogram(registerer prometheus.Registerer, collector *prometheus.HistogramVec) *prometheus.HistogramVec {
	if err := registerer.Register(collector); err != nil {
		var alreadyRegistered prometheus.AlreadyRegisteredError
		if errors.As(err, &alreadyRegistered) {
			if existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.HistogramVec); ok {
				return existing
			}
		}
		return nil
	}
	return collector
}

func (m *catalogPricingMetrics) recordResults(provider string, results []*publicv1.CatalogItemPricing) {
	if m.results == nil {
		return
	}
	for _, result := range results {
		if result == nil {
			continue
		}
		status := "UNAVAILABLE"
		switch result.GetStatus() {
		case publicv1.CatalogPricingStatus_CATALOG_PRICING_STATUS_CURRENT:
			status = "CURRENT"
		case publicv1.CatalogPricingStatus_CATALOG_PRICING_STATUS_STALE:
			status = "STALE"
		}
		m.results.WithLabelValues(provider, status).Inc()
	}
}

type catalogItemPricingClearer interface {
	ClearPricing()
}

func clearCatalogItemPricing(item catalogItemPricingClearer) {
	if item != nil {
		item.ClearPricing()
	}
}
