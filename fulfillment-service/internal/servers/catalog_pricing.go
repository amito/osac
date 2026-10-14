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
	catalogPricingRetryCount              = billing.ProviderReadRetryCount
	catalogPricingMaxConcurrency          = 4
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

type catalogPricingLookup struct {
	request      billing.CatalogPricingComponentRequest
	requestIndex int
}

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

	mu            sync.Mutex
	cache         map[catalogRateCardKey]catalogRateCardEntry
	inFlight      map[catalogRateCardKey]*catalogRateCardLoad
	rateCardSlots chan struct{}
	circuit       catalogPricingCircuitBreaker
	metrics       *catalogPricingMetrics
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
		rateCardSlots: make(chan struct{}, catalogPricingMaxConcurrency),
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

	lookupsByService := make(map[string][]catalogPricingLookup)
	expected := make([]int, len(requests))
	failed := make([]bool, len(requests))
	staleResults := make([]bool, len(requests))
	components := make([][]*publicv1.CatalogPriceComponent, len(requests))
	for index, request := range requests {
		if !request.Supported || request.Service == "" {
			failed[index] = true
			continue
		}
		componentRequests := catalogPricingComponents(request)
		if len(componentRequests) == 0 {
			failed[index] = true
			continue
		}
		expected[index] = len(componentRequests)
		for _, componentRequest := range componentRequests {
			lookupsByService[request.Service] = append(lookupsByService[request.Service], catalogPricingLookup{
				request: componentRequest, requestIndex: index,
			})
		}
	}
	services := make([]string, 0, len(lookupsByService))
	for service := range lookupsByService {
		services = append(services, service)
	}
	sort.Strings(services)
	for _, service := range services {
		rateItems, usedStale := r.getRateCardItems(ctx, account, service)
		if rateItems == nil {
			for _, lookup := range lookupsByService[service] {
				failed[lookup.requestIndex] = true
			}
			continue
		}
		for _, lookup := range lookupsByService[service] {
			matched, matchErr := r.resolveRateCardItem(rateItems, service, account.CurrencyCode, lookup.request.Dimensions)
			if matchErr != nil {
				failed[lookup.requestIndex] = true
				continue
			}
			amount, amountErr := billing.ComponentRateAmount(matched, lookup.request.Dimensions)
			if amountErr != nil {
				failed[lookup.requestIndex] = true
				continue
			}
			description := lookup.request.Description
			if description == "" {
				description = requests[lookup.requestIndex].Description
			}
			components[lookup.requestIndex] = append(components[lookup.requestIndex], publicv1.CatalogPriceComponent_builder{
				Amount: amount, Unit: string(matched.Unit), Description: description,
			}.Build())
			staleResults[lookup.requestIndex] = staleResults[lookup.requestIndex] || usedStale
		}
	}
	for index, request := range requests {
		if failed[index] || expected[index] == 0 || len(components[index]) != expected[index] {
			continue
		}
		status := publicv1.CatalogPricingStatus_CATALOG_PRICING_STATUS_CURRENT
		if staleResults[index] {
			status = publicv1.CatalogPricingStatus_CATALOG_PRICING_STATUS_STALE
		}
		results[index] = publicv1.CatalogItemPricing_builder{
			Status:       status,
			CurrencyCode: &account.CurrencyCode,
			PriceMayVary: request.PriceMayVary,
			Components:   components[index],
		}.Build()
	}
	return results
}

func catalogPricingComponents(request CatalogPricingRequest) []billing.CatalogPricingComponentRequest {
	components := make([]billing.CatalogPricingComponentRequest, 0, len(request.Components)+1)
	components = append(components, billing.CatalogPricingComponentRequest{
		Dimensions: request.Dimensions, Description: request.Description,
	})
	return append(components, request.Components...)
}

func (r *CatalogPricingResolver) resolveRateCardItem(
	items []billing.RateCardItem,
	service, currency string,
	dimensions map[string]string,
) (billing.RateCardItem, error) {
	item, err := billing.ResolveRateCardItem(items, service, currency, dimensions)
	if r == nil || r.metrics == nil || r.metrics.resolutionOutcomes == nil {
		return item, err
	}
	outcome := "resolved"
	switch {
	case errors.Is(err, billing.ErrRateCoverageGap):
		outcome = "coverage_gap"
	case errors.Is(err, billing.ErrAmbiguousRateCard):
		outcome = "ambiguous"
	case errors.Is(err, billing.ErrInvalidRateCard):
		outcome = "invalid_rate_card"
	case err != nil:
		outcome = "error"
	}
	r.metrics.resolutionOutcomes.WithLabelValues(r.providerKey, outcome).Inc()
	return item, err
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
	providerAccount, err := billing.RetryProviderRead(ctx, func(ctx context.Context) (billing.Account, error) {
		return r.accountStatus.GetAccount(ctx, account.ProviderAccountReference)
	})
	if err != nil {
		if ctx.Err() != nil {
			return grpcstatus.Error(grpccodes.DeadlineExceeded, "BILLING_COVERAGE_TIMEOUT")
		}
		return grpcstatus.Error(grpccodes.Unavailable, "BILLING_ACCOUNT_STATUS_UNAVAILABLE")
	}
	if providerAccount.Reference != account.ProviderAccountReference || providerAccount.State != billing.AccountStateActive {
		return grpcstatus.Error(grpccodes.FailedPrecondition, "BILLING_ACCOUNT_UNAVAILABLE")
	}

	byService := make(map[string][]billing.CatalogPricingComponentRequest)
	for _, request := range requests {
		if !request.Supported || request.Service == "" {
			return grpcstatus.Error(grpccodes.FailedPrecondition, "BILLING_CONFIGURATION_UNSUPPORTED")
		}
		components := catalogPricingComponents(request)
		if len(components) == 0 {
			return grpcstatus.Error(grpccodes.FailedPrecondition, "BILLING_CONFIGURATION_UNSUPPORTED")
		}
		byService[request.Service] = append(byService[request.Service], components...)
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
			_, err := r.resolveRateCardItem(rateItems, service, account.CurrencyCode, request.Dimensions)
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
	return r.listFreshRateCardItems(ctx, billing.RateCardQuery{
		AccountReference: account.ProviderAccountReference,
		Service:          service,
		CurrencyCode:     account.CurrencyCode,
	})
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
	loading := &catalogRateCardLoad{done: make(chan struct{})}
	r.inFlight[key] = loading
	r.mu.Unlock()

	query := billing.RateCardQuery{
		AccountReference: account.ProviderAccountReference,
		Service:          service,
		CurrencyCode:     account.CurrencyCode,
	}
	items, err := r.listFreshRateCardItems(ctx, query)
	if err == nil && items == nil {
		// A provider may represent an empty rate card with a nil slice. Keep that
		// distinct from nil returned after a failed lookup, which means unavailable.
		items = []billing.RateCardItem{}
	}
	return r.finishRateCardLoad(key, loading, items, err)
}

func (r *CatalogPricingResolver) finishRateCardLoad(
	key catalogRateCardKey,
	loading *catalogRateCardLoad,
	items []billing.RateCardItem,
	loadErr error,
) ([]billing.RateCardItem, bool) {
	r.mu.Lock()
	loading.items = items
	loading.err = loadErr
	if loadErr == nil {
		r.cache[key] = catalogRateCardEntry{items: items, fetchedAt: r.now()}
	}
	delete(r.inFlight, key)
	close(loading.done)
	if loadErr == nil {
		r.mu.Unlock()
		return items, false
	}
	entry, hasEntry := r.cache[key]
	r.mu.Unlock()
	if hasEntry {
		return entry.items, true
	}
	return nil, false
}

func listRateCardsWithRetries(ctx context.Context, provider billing.RateCardProvider, query billing.RateCardQuery) ([]billing.RateCardItem, error) {
	return billing.RetryProviderRead(ctx, func(ctx context.Context) ([]billing.RateCardItem, error) {
		return provider.ListRateCardItems(ctx, query)
	})
}

func (r *CatalogPricingResolver) listFreshRateCardItems(ctx context.Context, query billing.RateCardQuery) ([]billing.RateCardItem, error) {
	release, err := r.acquireRateCardSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	allowed, probe := r.circuit.allow(r.now())
	if !allowed {
		if r.metrics != nil && r.metrics.lookupOutcomes != nil {
			r.metrics.lookupOutcomes.WithLabelValues(r.providerKey, "circuit_open").Inc()
		}
		return nil, errors.New("billing pricing circuit is open")
	}
	startedAt := time.Now()
	items, err := listRateCardsWithRetries(ctx, r.provider, query)
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

func (r *CatalogPricingResolver) acquireRateCardSlot(ctx context.Context) (func(), error) {
	if r.rateCardSlots == nil {
		return func() {}, nil
	}
	select {
	case r.rateCardSlots <- struct{}{}:
		return func() { <-r.rateCardSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
	lookupOutcomes     *prometheus.CounterVec
	lookupDuration     *prometheus.HistogramVec
	resolutionOutcomes *prometheus.CounterVec
	results            *prometheus.CounterVec
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
	resolutionOutcomes := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "osac_billing_rate_resolution_outcomes_total",
		Help: "Provider rate resolution outcomes by configured provider and result.",
	}, []string{"provider", "outcome"})
	metrics.lookupOutcomes = registerCatalogPricingCounter(registerer, lookupOutcomes)
	metrics.lookupDuration = registerCatalogPricingHistogram(registerer, lookupDuration)
	metrics.resolutionOutcomes = registerCatalogPricingCounter(registerer, resolutionOutcomes)
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
