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
	"testing"

	"github.com/osac-project/osac/fulfillment-service/internal/auth"
	"github.com/osac-project/osac/fulfillment-service/internal/billing"
	"github.com/osac-project/osac/fulfillment-service/internal/collections"
	"github.com/osac-project/osac/fulfillment-service/internal/config"
	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

type costProviderStub struct {
	costs []billing.Cost
	query billing.CostQuery
	err   error
}

func (p *costProviderStub) ListCosts(_ context.Context, query billing.CostQuery) ([]billing.Cost, error) {
	p.query = query
	return p.costs, p.err
}

type costAccountStoreStub struct {
	account billing.AssignedAccount
	err     error
	tenant  string
}

func (s *costAccountStoreStub) GetAssignedAccount(_ context.Context, tenant string) (billing.AssignedAccount, error) {
	s.tenant = tenant
	return s.account, s.err
}

func newCostTenancyLogic(t *testing.T, projectVisibility ...string) *auth.MockTenancyLogic {
	t.Helper()
	controller := gomock.NewController(t)
	logic := auth.NewMockTenancyLogic(controller)
	visibilityBuilder := auth.NewVisibility()
	for _, project := range projectVisibility {
		visibilityBuilder.AddVisibleProject("tenant-a", project)
	}
	visibility, err := visibilityBuilder.Build()
	require.NoError(t, err)
	logic.EXPECT().DetermineVisibility(gomock.Any()).Return(visibility, nil).AnyTimes()
	logic.EXPECT().DetermineAssignableTenants(gomock.Any()).Return(collections.NewSet("tenant-a"), nil).AnyTimes()
	return logic
}

func newBillingCostsServer(t *testing.T, provider *costProviderStub, store *costAccountStoreStub, projects ...string) *BillingCostsServer {
	t.Helper()
	server, err := NewBillingCostsServer().
		SetLogger(newBillingTestLogger()).
		SetProvider("m360", provider).
		SetAccountStore(store).
		SetTenancyLogic(newCostTenancyLogic(t, projects...)).
		Build()
	require.NoError(t, err)
	return server
}

func TestBillingCostsListFiltersToVisibleProjects(t *testing.T) {
	provider := &costProviderStub{costs: []billing.Cost{
		{ProjectID: new("project-a"), ResourceID: new("vm-1"), Service: billing.ServiceComputeInstance, Amount: "4.50", CurrencyCode: "USD"},
		{ProjectID: new("project-b"), ResourceID: new("vm-2"), Service: billing.ServiceComputeInstance, Amount: "7.00", CurrencyCode: "USD"},
	}}
	store := &costAccountStoreStub{account: billing.AssignedAccount{
		ProviderKey: "m360", ProviderAccountReference: "native-account-1", CurrencyCode: "USD", ConnectionState: "CONNECTED",
	}}
	server := newBillingCostsServer(t, provider, store, "project-a")
	offset, limit := int32(0), int32(10)
	ctx := config.TenantIntoContext(context.Background(), "tenant-a")

	response, err := server.List(ctx, publicv1.BillingCostsListRequest_builder{
		FromDate: "2026-08-01", ToDate: "2026-08-31", Offset: &offset, Limit: &limit,
	}.Build())
	require.NoError(t, err)
	require.Equal(t, "tenant-a", store.tenant)
	require.Equal(t, "native-account-1", provider.query.AccountReference)
	require.Equal(t, "2026-08-01", provider.query.FromDate)
	require.Equal(t, "2026-08-31", provider.query.ToDate)
	require.Equal(t, int32(1), response.GetTotal())
	require.Equal(t, int32(1), response.GetSize())
	require.Equal(t, "project-a", response.GetItems()[0].GetProjectId())
	require.Equal(t, "vm-1", response.GetItems()[0].GetResourceId())
}

func TestBillingCostsRejectsUnattributedCostsForTenantCaller(t *testing.T) {
	provider := &costProviderStub{costs: []billing.Cost{{
		ResourceID: new("vm-1"), Service: billing.ServiceComputeInstance, Amount: "4.50", CurrencyCode: "USD",
	}}}
	store := &costAccountStoreStub{account: billing.AssignedAccount{
		ProviderKey: "m360", ProviderAccountReference: "native-account-1", CurrencyCode: "USD", ConnectionState: "CONNECTED",
	}}
	server := newBillingCostsServer(t, provider, store, "project-a")
	ctx := config.TenantIntoContext(context.Background(), "tenant-a")

	_, err := server.List(ctx, publicv1.BillingCostsListRequest_builder{FromDate: "2026-08-01", ToDate: "2026-08-31"}.Build())
	status, ok := grpcstatus.FromError(err)
	require.True(t, ok)
	require.Equal(t, grpccodes.FailedPrecondition, status.Code())
	require.Equal(t, "PROJECT_COST_ATTRIBUTION_UNAVAILABLE", status.Message())
}

func TestBillingCostsAllowsUnattributedCostsForTotalVisibility(t *testing.T) {
	provider := &costProviderStub{costs: []billing.Cost{{
		ResourceID: new("vm-1"), Service: billing.ServiceComputeInstance, Amount: "4.50", CurrencyCode: "USD",
	}}}
	store := &costAccountStoreStub{account: billing.AssignedAccount{
		ProviderKey: "m360", ProviderAccountReference: "native-account-1", CurrencyCode: "USD", ConnectionState: "CONNECTED",
	}}
	controller := gomock.NewController(t)
	tenancy := auth.NewMockTenancyLogic(controller)
	tenancy.EXPECT().DetermineVisibility(gomock.Any()).Return(auth.TotalVisibility(), nil).AnyTimes()
	tenancy.EXPECT().DetermineAssignableTenants(gomock.Any()).Return(collections.NewSet("tenant-a"), nil).AnyTimes()
	server, err := NewBillingCostsServer().
		SetLogger(newBillingTestLogger()).
		SetProvider("m360", provider).
		SetAccountStore(store).
		SetTenancyLogic(tenancy).
		Build()
	require.NoError(t, err)
	ctx := config.TenantIntoContext(context.Background(), "tenant-a")

	response, err := server.List(ctx, publicv1.BillingCostsListRequest_builder{
		FromDate: "2026-08-01", ToDate: "2026-08-31",
	}.Build())
	require.NoError(t, err)
	require.Len(t, response.GetItems(), 1)
	require.False(t, response.GetItems()[0].HasProjectId())
}

func TestBillingCostsRejectsProviderCurrencyMismatch(t *testing.T) {
	provider := &costProviderStub{costs: []billing.Cost{{
		ProjectID: new("project-a"), Service: billing.ServiceComputeInstance, Amount: "4.50", CurrencyCode: "EUR",
	}}}
	store := &costAccountStoreStub{account: billing.AssignedAccount{
		ProviderKey: "m360", ProviderAccountReference: "native-account-1", CurrencyCode: "USD", ConnectionState: "CONNECTED",
	}}
	server := newBillingCostsServer(t, provider, store, "project-a")
	ctx := config.TenantIntoContext(context.Background(), "tenant-a")

	_, err := server.List(ctx, publicv1.BillingCostsListRequest_builder{
		FromDate: "2026-08-01", ToDate: "2026-08-31",
	}.Build())
	status, ok := grpcstatus.FromError(err)
	require.True(t, ok)
	require.Equal(t, grpccodes.Unavailable, status.Code())
}

func TestBillingCostsSanitizesProviderErrors(t *testing.T) {
	provider := &costProviderStub{err: errors.New("provider response leaked native account reference")}
	store := &costAccountStoreStub{account: billing.AssignedAccount{
		ProviderKey: "m360", ProviderAccountReference: "native-account-1", CurrencyCode: "USD", ConnectionState: "CONNECTED",
	}}
	server := newBillingCostsServer(t, provider, store, "project-a")
	ctx := config.TenantIntoContext(context.Background(), "tenant-a")

	_, err := server.List(ctx, publicv1.BillingCostsListRequest_builder{FromDate: "2026-08-01", ToDate: "2026-08-31"}.Build())
	status, ok := grpcstatus.FromError(err)
	require.True(t, ok)
	require.Equal(t, grpccodes.Unavailable, status.Code())
	require.NotContains(t, status.Message(), "native account reference")
}
