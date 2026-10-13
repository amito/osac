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
	"log/slog"
	"strings"

	"github.com/osac-project/osac/fulfillment-service/internal/auth"
	"github.com/osac-project/osac/fulfillment-service/internal/billing"
	"github.com/osac-project/osac/fulfillment-service/internal/config"
	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

type BillingPricingServers struct {
	PricingQueries *PricingQueriesServer
	PriceEstimates *PriceEstimatesServer
}

type billingPricingService struct {
	logger         *slog.Logger
	tenancy        auth.TenancyLogic
	resolver       *CatalogPricingResolver
	computeItems   ComputeCatalogItemGetter
	clusterItems   ClusterCatalogItemGetter
	bareMetalItems BareMetalCatalogItemGetter
}

type PricingQueriesServer struct {
	publicv1.UnimplementedPricingQueriesServer
	service *billingPricingService
}

type PriceEstimatesServer struct {
	publicv1.UnimplementedPriceEstimatesServer
	service *billingPricingService
}

type ComputeCatalogItemGetter interface {
	Get(context.Context, *publicv1.ComputeInstanceCatalogItemsGetRequest) (*publicv1.ComputeInstanceCatalogItemsGetResponse, error)
}

type ClusterCatalogItemGetter interface {
	Get(context.Context, *publicv1.ClusterCatalogItemsGetRequest) (*publicv1.ClusterCatalogItemsGetResponse, error)
}

type BareMetalCatalogItemGetter interface {
	Get(context.Context, *publicv1.BareMetalInstanceCatalogItemsGetRequest) (*publicv1.BareMetalInstanceCatalogItemsGetResponse, error)
}

var (
	_ publicv1.PricingQueriesServer = (*PricingQueriesServer)(nil)
	_ publicv1.PriceEstimatesServer = (*PriceEstimatesServer)(nil)
)

func NewBillingPricingServers(
	logger *slog.Logger,
	tenancy auth.TenancyLogic,
	resolver *CatalogPricingResolver,
	computeItems ComputeCatalogItemGetter,
	clusterItems ClusterCatalogItemGetter,
	bareMetalItems BareMetalCatalogItemGetter,
) (*BillingPricingServers, error) {
	if logger == nil {
		return nil, errors.New("logger is mandatory")
	}
	if tenancy == nil {
		return nil, errors.New("tenancy logic is mandatory")
	}
	service := &billingPricingService{
		logger:         logger,
		tenancy:        tenancy,
		resolver:       resolver,
		computeItems:   computeItems,
		clusterItems:   clusterItems,
		bareMetalItems: bareMetalItems,
	}
	return &BillingPricingServers{
		PricingQueries: &PricingQueriesServer{service: service},
		PriceEstimates: &PriceEstimatesServer{service: service},
	}, nil
}

func (s *PricingQueriesServer) Create(ctx context.Context,
	request *publicv1.PricingQueriesCreateRequest,
) (*publicv1.PricingQueriesCreateResponse, error) {
	if request == nil || request.GetConfiguration() == nil {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "resource configuration is required")
	}
	currency, components, err := s.service.resolve(ctx, request.GetTenantId(), request.GetConfiguration())
	if err != nil {
		return nil, err
	}
	result := make([]*publicv1.PricingComponentResult, 0, len(components))
	for _, component := range components {
		coverage := publicv1.PricingCoverageState_PRICING_COVERAGE_STATE_UNAVAILABLE
		if component.Gap {
			coverage = publicv1.PricingCoverageState_PRICING_COVERAGE_STATE_GAP
		} else if component.Rate != nil {
			coverage = publicv1.PricingCoverageState_PRICING_COVERAGE_STATE_COVERED
			if component.Stale {
				coverage = publicv1.PricingCoverageState_PRICING_COVERAGE_STATE_STALE
			}
		}
		item := publicv1.PricingComponentResult_builder{
			Service:     component.Request.Service,
			Description: component.Request.Description,
			Coverage:    coverage,
		}
		if component.Rate != nil {
			amount := component.Rate.Amount
			unit := string(component.Rate.Unit)
			item.Amount = &amount
			item.Unit = &unit
		}
		result = append(result, item.Build())
	}
	return publicv1.PricingQueriesCreateResponse_builder{
		CurrencyCode: currency,
		Components:   result,
	}.Build(), nil
}

func (s *PriceEstimatesServer) Create(ctx context.Context,
	request *publicv1.PriceEstimatesCreateRequest,
) (*publicv1.PriceEstimatesCreateResponse, error) {
	if request == nil || request.GetConfiguration() == nil {
		return nil, grpcstatus.Error(grpccodes.InvalidArgument, "resource configuration is required")
	}
	currency, components, err := s.service.resolve(ctx, request.GetTenantId(), request.GetConfiguration())
	if err != nil {
		return nil, err
	}
	if len(components) != 1 || components[0].Rate == nil {
		if len(components) != 0 && components[0].Gap {
			return nil, grpcstatus.Error(grpccodes.FailedPrecondition, "BILLING_COVERAGE_GAP")
		}
		return nil, grpcstatus.Error(grpccodes.FailedPrecondition, "BILLING_CONFIGURATION_UNSUPPORTED")
	}
	component := components[0]
	rate := component.Rate
	hourly, monthly, err := billing.NormalizeRecurringRate(rate.Amount, rate.Unit)
	if err != nil {
		return nil, grpcstatus.Error(grpccodes.Unimplemented, "PROVIDER_RATE_UNIT_UNSUPPORTED")
	}
	status := publicv1.PriceEstimateStatus_PRICE_ESTIMATE_STATUS_CURRENT
	if component.Stale {
		status = publicv1.PriceEstimateStatus_PRICE_ESTIMATE_STATUS_STALE
	}
	return publicv1.PriceEstimatesCreateResponse_builder{
		CurrencyCode: currency,
		Components: []*publicv1.PriceEstimateComponent{publicv1.PriceEstimateComponent_builder{
			Service:       component.Request.Service,
			Description:   component.Request.Description,
			RateAmount:    rate.Amount,
			RateUnit:      string(rate.Unit),
			HourlyAmount:  &hourly,
			MonthlyAmount: &monthly,
		}.Build()},
		HourlyAmount:  hourly,
		MonthlyAmount: monthly,
		Status:        status,
	}.Build(), nil
}

func (s *billingPricingService) resolve(
	ctx context.Context,
	requestedTenant string,
	configuration *publicv1.BillingPricingConfiguration,
) (string, []resolvedPricingComponent, error) {
	tenantID := strings.TrimSpace(requestedTenant)
	if tenantID == "" {
		tenantID = config.TenantFromContext(ctx)
	}
	if tenantID == "" {
		return "", nil, grpcstatus.Error(grpccodes.InvalidArgument, "tenant_id is required")
	}
	if err := auth.ValidateTenantAssignment(ctx, s.tenancy, tenantID); err != nil {
		return "", nil, convertTenantErrorToGRPC(ctx, err, s.logger, tenantID)
	}
	if s.resolver == nil {
		return "", nil, grpcstatus.Error(grpccodes.FailedPrecondition, "BILLING_NOT_CONFIGURED")
	}
	request, err := s.pricingRequestForConfiguration(withoutCatalogPricing(ctx), configuration)
	if err != nil {
		return "", nil, err
	}
	return s.resolver.ResolveSelectedConfiguration(ctx, tenantID, []CatalogPricingRequest{request})
}

func (s *billingPricingService) pricingRequestForConfiguration(
	ctx context.Context,
	configuration *publicv1.BillingPricingConfiguration,
) (CatalogPricingRequest, error) {
	if spec := configuration.GetComputeInstance(); spec != nil {
		if s.computeItems == nil {
			return CatalogPricingRequest{}, grpcstatus.Error(grpccodes.Unimplemented, "VMaaS is not enabled")
		}
		if spec.GetCatalogItem().GetId() == "" {
			return CatalogPricingRequest{}, grpcstatus.Error(grpccodes.InvalidArgument, "compute instance catalog item is required")
		}
		response, err := s.computeItems.Get(ctx, publicv1.ComputeInstanceCatalogItemsGetRequest_builder{Id: spec.GetCatalogItem().GetId()}.Build())
		if err != nil {
			return CatalogPricingRequest{}, catalogPricingItemError(err)
		}
		if response == nil {
			return CatalogPricingRequest{}, grpcstatus.Error(grpccodes.NotFound, "catalog item was not found")
		}
		item := response.GetObject()
		if item == nil {
			return CatalogPricingRequest{}, grpcstatus.Error(grpccodes.NotFound, "catalog item was not found")
		}
		requests := computeCatalogPricingRequests([]*publicv1.ComputeInstanceCatalogItem{item})
		request, err := billing.ApplyComputeInstancePricingConfiguration(requests[0], spec, item.GetFields())
		if err != nil {
			return CatalogPricingRequest{}, grpcstatus.Error(grpccodes.InvalidArgument, "resource configuration is not allowed by the catalog item")
		}
		return request, nil
	}
	if spec := configuration.GetCluster(); spec != nil {
		if s.clusterItems == nil {
			return CatalogPricingRequest{}, grpcstatus.Error(grpccodes.Unimplemented, "CaaS is not enabled")
		}
		if spec.GetCatalogItem().GetId() == "" {
			return CatalogPricingRequest{}, grpcstatus.Error(grpccodes.InvalidArgument, "cluster catalog item is required")
		}
		response, err := s.clusterItems.Get(ctx, publicv1.ClusterCatalogItemsGetRequest_builder{Id: spec.GetCatalogItem().GetId()}.Build())
		if err != nil {
			return CatalogPricingRequest{}, catalogPricingItemError(err)
		}
		if response == nil {
			return CatalogPricingRequest{}, grpcstatus.Error(grpccodes.NotFound, "catalog item was not found")
		}
		item := response.GetObject()
		if item == nil {
			return CatalogPricingRequest{}, grpcstatus.Error(grpccodes.NotFound, "catalog item was not found")
		}
		requests := clusterCatalogPricingRequests([]*publicv1.ClusterCatalogItem{item})
		request, err := billing.ApplyClusterPricingConfiguration(requests[0], spec, item.GetFields())
		if err != nil {
			return CatalogPricingRequest{}, grpcstatus.Error(grpccodes.InvalidArgument, "resource configuration is not allowed by the catalog item")
		}
		return request, nil
	}
	if spec := configuration.GetBareMetalInstance(); spec != nil {
		if s.bareMetalItems == nil {
			return CatalogPricingRequest{}, grpcstatus.Error(grpccodes.Unimplemented, "BMaaS is not enabled")
		}
		if spec.GetCatalogItem().GetId() == "" {
			return CatalogPricingRequest{}, grpcstatus.Error(grpccodes.InvalidArgument, "bare metal catalog item is required")
		}
		response, err := s.bareMetalItems.Get(ctx, publicv1.BareMetalInstanceCatalogItemsGetRequest_builder{Id: spec.GetCatalogItem().GetId()}.Build())
		if err != nil {
			return CatalogPricingRequest{}, catalogPricingItemError(err)
		}
		if response == nil {
			return CatalogPricingRequest{}, grpcstatus.Error(grpccodes.NotFound, "catalog item was not found")
		}
		item := response.GetObject()
		if item == nil {
			return CatalogPricingRequest{}, grpcstatus.Error(grpccodes.NotFound, "catalog item was not found")
		}
		requests := bareMetalCatalogPricingRequests([]*publicv1.BareMetalInstanceCatalogItem{item})
		request, err := billing.ApplyBareMetalPricingConfiguration(requests[0], spec, item.GetFields())
		if err != nil {
			return CatalogPricingRequest{}, grpcstatus.Error(grpccodes.InvalidArgument, "resource configuration is not allowed by the catalog item")
		}
		return request, nil
	}
	return CatalogPricingRequest{}, grpcstatus.Error(grpccodes.InvalidArgument, "resource configuration is required")
}

func catalogPricingItemError(err error) error {
	switch grpcstatus.Code(err) {
	case grpccodes.NotFound, grpccodes.PermissionDenied:
		return grpcstatus.Error(grpccodes.NotFound, "catalog item was not found")
	default:
		return grpcstatus.Error(grpccodes.Internal, "failed to resolve catalog item")
	}
}
