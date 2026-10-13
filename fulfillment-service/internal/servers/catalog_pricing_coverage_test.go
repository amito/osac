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
	"strings"
	"testing"

	"github.com/osac-project/osac/fulfillment-service/internal/billing"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCatalogPricingResolverCheckCoverageUsesFreshProviderRates(t *testing.T) {
	store := &catalogPricingAccountStoreStub{account: billing.AssignedAccount{
		ProviderKey: "m360", ProviderAccountReference: "native-account", CurrencyCode: "USD", ConnectionState: "CONNECTED",
	}}
	provider := &catalogPricingRateCardProviderStub{items: []billing.RateCardItem{{
		Service: billing.ServiceComputeInstance, CurrencyCode: "USD", Amount: "3.00", Unit: billing.RateUnitHour,
		Conditions: []billing.RateCondition{{Dimension: "image_ref", Operator: billing.RateMatchEquals, Value: "fedora-16"}},
	}}}
	resolver := NewCatalogPricingResolver("m360", provider, store)
	request := CatalogPricingRequest{Service: billing.ServiceComputeInstance, Dimensions: map[string]string{"image_ref": "fedora-16"}, Supported: true}

	if got := resolver.Resolve(context.Background(), "tenant-a", []CatalogPricingRequest{request}); got[0].GetComponents()[0].GetAmount() != "3.00" {
		t.Fatalf("Resolve() = %v, want cached current rate", got)
	}
	if err := resolver.CheckCoverage(context.Background(), "tenant-a", []CatalogPricingRequest{request}); err != nil {
		t.Fatalf("CheckCoverage() error = %v, want current rate coverage", err)
	}
	provider.items = []billing.RateCardItem{{
		Service: billing.ServiceComputeInstance, CurrencyCode: "USD", Amount: "4.00", Unit: billing.RateUnitHour,
		Conditions: []billing.RateCondition{{Dimension: "image_ref", Operator: billing.RateMatchEquals, Value: "fedora-14"}},
	}}

	err := resolver.CheckCoverage(context.Background(), "tenant-a", []CatalogPricingRequest{request})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "BILLING_COVERAGE_GAP") {
		t.Fatalf("CheckCoverage() error = %v, want failed precondition for missing current coverage", err)
	}
	if provider.calls != 3 {
		t.Fatalf("provider calls = %d, want fresh provider lookups despite catalog cache", provider.calls)
	}
}

func TestCatalogPricingResolverCheckCoverageFailsClosedOnProviderFailure(t *testing.T) {
	provider := &catalogPricingRateCardProviderStub{err: errors.New("private provider error")}
	resolver := NewCatalogPricingResolver("m360", provider, &catalogPricingAccountStoreStub{account: billing.AssignedAccount{
		ProviderKey: "m360", ProviderAccountReference: "native-account", CurrencyCode: "USD", ConnectionState: "CONNECTED",
	}})

	err := resolver.CheckCoverage(context.Background(), "tenant-a", []CatalogPricingRequest{{
		Service: billing.ServiceComputeInstance, Supported: true,
	}})
	if status.Code(err) != codes.Unavailable || strings.Contains(status.Convert(err).Message(), "private provider error") {
		t.Fatalf("CheckCoverage() error = %v, want sanitized unavailable error", err)
	}
	if provider.calls != catalogPricingRetryCount+1 {
		t.Fatalf("provider calls = %d, want %d retry attempts", provider.calls, catalogPricingRetryCount+1)
	}
}

func TestCatalogPricingResolverCheckCoverageRequiresConnectedAssignment(t *testing.T) {
	provider := &catalogPricingRateCardProviderStub{}
	resolver := NewCatalogPricingResolver("m360", provider, &catalogPricingAccountStoreStub{err: billing.ErrTenantBillingAssignmentNotFound})

	err := resolver.CheckCoverage(context.Background(), "tenant-a", []CatalogPricingRequest{{Service: billing.ServiceComputeInstance, Supported: true}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("CheckCoverage() error = %v, want failed precondition without a linked account", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls = %d, want no provider calls without assignment", provider.calls)
	}
}

func TestCatalogPricingResolverCheckCoverageRejectsUnsupportedConfiguration(t *testing.T) {
	provider := &catalogPricingRateCardProviderStub{}
	resolver := NewCatalogPricingResolver("m360", provider, &catalogPricingAccountStoreStub{account: billing.AssignedAccount{
		ProviderKey: "m360", ProviderAccountReference: "native-account", CurrencyCode: "USD", ConnectionState: "CONNECTED",
	}})

	err := resolver.CheckCoverage(context.Background(), "tenant-a", []CatalogPricingRequest{{
		Service: billing.ServiceCluster, Supported: false,
	}})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "BILLING_CONFIGURATION_UNSUPPORTED") {
		t.Fatalf("CheckCoverage() error = %v, want failed precondition for unsupported configuration", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls = %d, want no provider call for unsupported configuration", provider.calls)
	}
}
