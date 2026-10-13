/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package servers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/osac-project/osac/fulfillment-service/internal/auth"
	"github.com/osac-project/osac/fulfillment-service/internal/billing"
	"github.com/osac-project/osac/fulfillment-service/internal/collections"
	"github.com/osac-project/osac/fulfillment-service/internal/config"
	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPriceEstimateUsesSelectedConfiguration(t *testing.T) {
	t.Parallel()

	provider := &billingPricingTestProvider{items: []billing.RateCardItem{{
		Service:      billing.ServiceComputeInstance,
		CurrencyCode: "USD",
		Conditions: []billing.RateCondition{
			{Dimension: "image_ref", Operator: billing.RateMatchEquals, Value: "fedora-16"},
			{Dimension: "instance_type", Operator: billing.RateMatchEquals, Value: "large"},
		},
		Amount: "10.25",
		Unit:   billing.RateUnitMonth,
	}}}
	computeItems := &billingPricingTestComputeCatalog{item: billingPricingTestComputeCatalogItem()}
	query, estimate := newBillingPricingTestServers(t, provider, computeItems, auth.AllTenants)

	configuration := billingPricingTestConfiguration()
	ctx := config.TenantIntoContext(context.Background(), "tenant-a")
	response, err := estimate.Create(ctx, publicv1.PriceEstimatesCreateRequest_builder{
		Configuration: configuration,
	}.Build())
	if err != nil {
		t.Fatalf("PriceEstimates.Create() error = %v", err)
	}
	if response.GetCurrencyCode() != "USD" || response.GetStatus() != publicv1.PriceEstimateStatus_PRICE_ESTIMATE_STATUS_CURRENT {
		t.Fatalf("estimate metadata = (%q, %v), want USD and CURRENT", response.GetCurrencyCode(), response.GetStatus())
	}
	if response.GetMonthlyAmount() != "10.25" || response.GetHourlyAmount() != "0.014041" {
		t.Fatalf("normalized estimate = (%q/hour, %q/month), want (0.014041, 10.25)", response.GetHourlyAmount(), response.GetMonthlyAmount())
	}
	if len(response.GetComponents()) != 1 || response.GetComponents()[0].GetRateAmount() != "10.25" || response.GetComponents()[0].GetRateUnit() != "month" {
		t.Fatalf("estimate components = %v, want the provider's monthly rate preserved", response.GetComponents())
	}
	if computeItems.calls != 1 || provider.calls != 1 {
		t.Fatalf("catalog/provider calls = (%d, %d), want (1, 1)", computeItems.calls, provider.calls)
	}
	if len(query.service.resolver.cache) != 1 {
		t.Fatalf("rate card cache size = %d, want 1", len(query.service.resolver.cache))
	}
}

func TestPricingQueryReportsCoverageGapWithoutAmount(t *testing.T) {
	t.Parallel()

	provider := &billingPricingTestProvider{}
	computeItems := &billingPricingTestComputeCatalog{item: billingPricingTestComputeCatalogItem()}
	query, _ := newBillingPricingTestServers(t, provider, computeItems, auth.AllTenants)

	response, err := query.Create(config.TenantIntoContext(context.Background(), "tenant-a"), publicv1.PricingQueriesCreateRequest_builder{
		Configuration: billingPricingTestConfiguration(),
	}.Build())
	if err != nil {
		t.Fatalf("PricingQueries.Create() error = %v", err)
	}
	if response.GetCurrencyCode() != "USD" || len(response.GetComponents()) != 1 {
		t.Fatalf("query response = %v, want one USD component", response)
	}
	component := response.GetComponents()[0]
	if component.GetCoverage() != publicv1.PricingCoverageState_PRICING_COVERAGE_STATE_GAP {
		t.Fatalf("coverage = %v, want GAP", component.GetCoverage())
	}
	if component.HasAmount() || component.HasUnit() {
		t.Fatalf("gap result includes a price: %v", component)
	}
}

func TestPricingQueryRejectsTenantOutsideAssignmentScopeBeforeLookups(t *testing.T) {
	t.Parallel()

	provider := &billingPricingTestProvider{items: []billing.RateCardItem{{Service: billing.ServiceComputeInstance, CurrencyCode: "USD", Amount: "1", Unit: billing.RateUnitHour}}}
	computeItems := &billingPricingTestComputeCatalog{item: billingPricingTestComputeCatalogItem()}
	query, _ := newBillingPricingTestServers(t, provider, computeItems, collections.NewSet[string]())

	_, err := query.Create(context.Background(), publicv1.PricingQueriesCreateRequest_builder{
		TenantId:      stringPointer("tenant-b"),
		Configuration: billingPricingTestConfiguration(),
	}.Build())
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("PricingQueries.Create() error = %v, want PermissionDenied", err)
	}
	if computeItems.calls != 0 || provider.calls != 0 {
		t.Fatalf("catalog/provider calls = (%d, %d), want no lookups before tenant authorization", computeItems.calls, provider.calls)
	}
}

func TestPriceEstimateMarksRetainedRateStaleAfterRefreshFailure(t *testing.T) {
	t.Parallel()

	provider := &billingPricingTestProvider{items: []billing.RateCardItem{{
		Service:      billing.ServiceComputeInstance,
		CurrencyCode: "USD",
		Conditions: []billing.RateCondition{{
			Dimension: "image_ref",
			Operator:  billing.RateMatchEquals,
			Value:     "fedora-16",
		}},
		Amount: "4.50",
		Unit:   billing.RateUnitHour,
	}}}
	_, estimate := newBillingPricingTestServers(t, provider, &billingPricingTestComputeCatalog{item: billingPricingTestComputeCatalogItem()}, auth.AllTenants)
	configuration := billingPricingTestConfiguration()
	ctx := config.TenantIntoContext(context.Background(), "tenant-a")
	if _, err := estimate.Create(ctx, publicv1.PriceEstimatesCreateRequest_builder{Configuration: configuration}.Build()); err != nil {
		t.Fatalf("initial PriceEstimates.Create() error = %v", err)
	}

	provider.err = errors.New("provider unavailable")
	now := time.Now().Add(catalogPricingCacheTTL + time.Second)
	estimate.service.resolver.now = func() time.Time { return now }
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	response, err := estimate.Create(canceled, publicv1.PriceEstimatesCreateRequest_builder{Configuration: configuration}.Build())
	if err != nil {
		t.Fatalf("stale PriceEstimates.Create() error = %v", err)
	}
	if response.GetStatus() != publicv1.PriceEstimateStatus_PRICE_ESTIMATE_STATUS_STALE || response.GetComponents()[0].GetRateAmount() != "4.50" {
		t.Fatalf("stale estimate = %v, want retained 4.50 rate marked STALE", response)
	}
}

func TestPricingQueryHidesCatalogItemsThatAreNotVisible(t *testing.T) {
	t.Parallel()

	provider := &billingPricingTestProvider{items: []billing.RateCardItem{{Service: billing.ServiceComputeInstance, CurrencyCode: "USD", Amount: "1", Unit: billing.RateUnitHour}}}
	computeItems := &billingPricingTestComputeCatalog{err: status.Error(codes.PermissionDenied, "catalog item is hidden")}
	query, _ := newBillingPricingTestServers(t, provider, computeItems, auth.AllTenants)

	_, err := query.Create(config.TenantIntoContext(context.Background(), "tenant-a"), publicv1.PricingQueriesCreateRequest_builder{
		Configuration: billingPricingTestConfiguration(),
	}.Build())
	if status.Code(err) != codes.NotFound {
		t.Fatalf("PricingQueries.Create() error = %v, want hidden catalog item as NotFound", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls = %d, want no lookup for a hidden catalog item", provider.calls)
	}
}

func TestPriceEstimateFailsWhenBillingIsNotConfigured(t *testing.T) {
	t.Parallel()

	computeItems := &billingPricingTestComputeCatalog{item: billingPricingTestComputeCatalogItem()}
	servers, err := NewBillingPricingServers(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		billingPricingTestTenancy{assignable: auth.AllTenants},
		nil,
		computeItems,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("NewBillingPricingServers() error = %v", err)
	}
	_, err = servers.PriceEstimates.Create(config.TenantIntoContext(context.Background(), "tenant-a"), publicv1.PriceEstimatesCreateRequest_builder{
		Configuration: billingPricingTestConfiguration(),
	}.Build())
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "BILLING_NOT_CONFIGURED" {
		t.Fatalf("PriceEstimates.Create() error = %v, want BILLING_NOT_CONFIGURED", err)
	}
	if computeItems.calls != 0 {
		t.Fatalf("catalog calls = %d, want no catalog reads when billing is disabled", computeItems.calls)
	}
}

type billingPricingTestTenancy struct {
	assignable collections.Set[string]
}

func (t billingPricingTestTenancy) DetermineAssignableTenants(context.Context) (collections.Set[string], error) {
	return t.assignable, nil
}

func (billingPricingTestTenancy) DetermineDefaultTenant(context.Context) (string, error) {
	return "tenant-a", nil
}

func (billingPricingTestTenancy) DetermineVisibility(context.Context) (*auth.Visibility, error) {
	return auth.TotalVisibility(), nil
}

type billingPricingTestAccounts struct{}

func (billingPricingTestAccounts) GetAssignedAccount(context.Context, string) (billing.AssignedAccount, error) {
	return billing.AssignedAccount{
		ProviderKey:              "m360",
		ProviderAccountReference: "provider-account-1",
		CurrencyCode:             "USD",
		ConnectionState:          "CONNECTED",
	}, nil
}

type billingPricingTestProvider struct {
	items []billing.RateCardItem
	err   error
	calls int
}

func (p *billingPricingTestProvider) ListRateCardItems(ctx context.Context, _ billing.RateCardQuery) ([]billing.RateCardItem, error) {
	p.calls++
	if p.err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, p.err
	}
	return p.items, nil
}

type billingPricingTestComputeCatalog struct {
	item  *publicv1.ComputeInstanceCatalogItem
	err   error
	calls int
}

func (c *billingPricingTestComputeCatalog) Get(context.Context, *publicv1.ComputeInstanceCatalogItemsGetRequest) (*publicv1.ComputeInstanceCatalogItemsGetResponse, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return publicv1.ComputeInstanceCatalogItemsGetResponse_builder{Object: c.item}.Build(), nil
}

func newBillingPricingTestServers(
	t *testing.T,
	provider *billingPricingTestProvider,
	computeItems *billingPricingTestComputeCatalog,
	assignable collections.Set[string],
) (*PricingQueriesServer, *PriceEstimatesServer) {
	t.Helper()
	resolver := NewCatalogPricingResolver("m360", provider, billingPricingTestAccounts{})
	servers, err := NewBillingPricingServers(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		billingPricingTestTenancy{assignable: assignable},
		resolver,
		computeItems,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("NewBillingPricingServers() error = %v", err)
	}
	return servers.PricingQueries, servers.PriceEstimates
}

func billingPricingTestComputeCatalogItem() *publicv1.ComputeInstanceCatalogItem {
	return publicv1.ComputeInstanceCatalogItem_builder{
		Title: "Small VM",
		Fields: publicv1.ComputeInstanceCatalogItemFields_builder{
			DiskImage: publicv1.DiskImageReferenceFieldPolicy_builder{
				Locked: publicv1.DiskImageReference_builder{Name: "fedora-16"}.Build(),
			}.Build(),
			InstanceType: publicv1.InstanceTypeReferenceFieldPolicy_builder{
				Editable: publicv1.EditableInstanceTypeReferenceField_builder{
					DefaultValue: publicv1.InstanceTypeReference_builder{Name: "small"}.Build(),
				}.Build(),
			}.Build(),
		}.Build(),
	}.Build()
}

func billingPricingTestConfiguration() *publicv1.BillingPricingConfiguration {
	return publicv1.BillingPricingConfiguration_builder{
		ComputeInstance: publicv1.ComputeInstanceSpec_builder{
			CatalogItem:  publicv1.ComputeInstanceCatalogItemReference_builder{Id: "catalog-vm"}.Build(),
			DiskImage:    publicv1.DiskImageReference_builder{Name: "fedora-16"}.Build(),
			InstanceType: publicv1.InstanceTypeReference_builder{Name: "large"}.Build(),
		}.Build(),
	}.Build()
}

func stringPointer(value string) *string {
	return &value
}
