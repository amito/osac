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
	"testing"

	privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"
)

func TestClearCatalogItemPricing(t *testing.T) {
	pricing := privatev1.CatalogItemPricing_builder{
		Status: privatev1.CatalogPricingStatus_CATALOG_PRICING_STATUS_CURRENT,
	}.Build()
	computeItem := privatev1.ComputeInstanceCatalogItem_builder{Pricing: pricing}.Build()
	clusterItem := privatev1.ClusterCatalogItem_builder{Pricing: pricing}.Build()
	bareMetalItem := privatev1.BareMetalInstanceCatalogItem_builder{Pricing: pricing}.Build()
	tests := []struct {
		name       string
		item       interface{ ClearPricing() }
		getPricing func() *privatev1.CatalogItemPricing
	}{
		{
			name:       "compute instance",
			item:       computeItem,
			getPricing: computeItem.GetPricing,
		},
		{
			name:       "cluster",
			item:       clusterItem,
			getPricing: clusterItem.GetPricing,
		},
		{
			name:       "bare metal instance",
			item:       bareMetalItem,
			getPricing: bareMetalItem.GetPricing,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearCatalogItemPricing(test.item)
			if got := test.getPricing(); got != nil {
				t.Fatalf("pricing = %v, want nil", got)
			}
		})
	}
}
