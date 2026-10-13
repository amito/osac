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
	"strconv"

	"github.com/osac-project/osac/fulfillment-service/internal/billing"
	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
)

func computeCatalogPricingRequests(items []*publicv1.ComputeInstanceCatalogItem) []CatalogPricingRequest {
	requests := make([]CatalogPricingRequest, len(items))
	for i, item := range items {
		request := CatalogPricingRequest{
			Service:     billing.ServiceComputeInstance,
			Dimensions:  make(map[string]string),
			Description: catalogPriceDescription(item.GetTitle(), "Compute instance"),
			Supported:   true,
		}
		if fields := item.GetFields(); fields != nil {
			request.PriceMayVary = addDiskImagePricingDimension(request.Dimensions, "image_ref", fields.GetDiskImage()) || request.PriceMayVary
			request.PriceMayVary = addInstanceTypePricingDimension(request.Dimensions, "instance_type", fields.GetInstanceType()) || request.PriceMayVary
			if bootDisk := fields.GetBootDisk(); bootDisk != nil {
				request.PriceMayVary = addInt32PricingDimension(request.Dimensions, "boot_disk_size_gib", bootDisk.GetSizeGib()) || request.PriceMayVary
				request.PriceMayVary = addStorageTierPricingDimension(request.Dimensions, "storage_tier", bootDisk.GetStorageTier()) || request.PriceMayVary
			}
			request.PriceMayVary = addComputeAdditionalDiskPricingDimensions(&request, fields.GetAdditionalDisks()) || request.PriceMayVary
		}
		requests[i] = request
	}
	return requests
}

func clusterCatalogPricingRequests(items []*publicv1.ClusterCatalogItem) []CatalogPricingRequest {
	requests := make([]CatalogPricingRequest, len(items))
	for i, item := range items {
		request := CatalogPricingRequest{
			Service:     billing.ServiceCluster,
			Dimensions:  make(map[string]string),
			Description: catalogPriceDescription(item.GetTitle(), "Cluster"),
			Supported:   true,
		}
		if template := item.GetTemplate(); template != nil && template.GetName() != "" {
			request.Dimensions["cluster_template"] = template.GetName()
		}
		if fields := item.GetFields(); fields != nil {
			request.PriceMayVary = addClusterVersionPricingDimension(request.Dimensions, "release_image", fields.GetVersion())
			request.PriceMayVary = addClusterNodeSetPricingDimensions(&request, fields.GetNodeSets()) || request.PriceMayVary
		}
		requests[i] = request
	}
	return requests
}

func bareMetalCatalogPricingRequests(items []*publicv1.BareMetalInstanceCatalogItem) []CatalogPricingRequest {
	requests := make([]CatalogPricingRequest, len(items))
	for i, item := range items {
		request := CatalogPricingRequest{
			Service:     billing.ServiceBareMetalInstance,
			Dimensions:  make(map[string]string),
			Description: catalogPriceDescription(item.GetTitle(), "Bare metal instance"),
			Supported:   true,
		}
		if metadata := item.GetMetadata(); metadata != nil && metadata.GetName() != "" {
			request.Dimensions["catalog_item"] = metadata.GetName()
		}
		if fields := item.GetFields(); fields != nil {
			request.PriceMayVary = addBareMetalInstanceTypePricingDimension(request.Dimensions, "bm_instance_type", fields.GetInstanceType())
			request.PriceMayVary = addDiskImagePricingDimension(request.Dimensions, "image_ref", fields.GetDiskImage()) || request.PriceMayVary
		}
		requests[i] = request
	}
	return requests
}

func addDiskImagePricingDimension(dimensions map[string]string, dimension string, policy *publicv1.DiskImageReferenceFieldPolicy) bool {
	if policy == nil {
		return false
	}
	locked := policy.GetLocked()
	editable := policy.GetEditable()
	var defaultValue *publicv1.DiskImageReference
	if editable != nil {
		defaultValue = editable.GetDefaultValue()
	}
	return addReferencePricingDimension(dimensions, dimension, locked.GetName(), defaultValue.GetName(), editable != nil, defaultValue != nil)
}

func addInstanceTypePricingDimension(dimensions map[string]string, dimension string, policy *publicv1.InstanceTypeReferenceFieldPolicy) bool {
	if policy == nil {
		return false
	}
	locked := policy.GetLocked()
	editable := policy.GetEditable()
	var defaultValue *publicv1.InstanceTypeReference
	if editable != nil {
		defaultValue = editable.GetDefaultValue()
	}
	return addReferencePricingDimension(dimensions, dimension, locked.GetName(), defaultValue.GetName(), editable != nil, defaultValue != nil)
}

func addStorageTierPricingDimension(dimensions map[string]string, dimension string, policy *publicv1.StorageTierReferenceFieldPolicy) bool {
	if policy == nil {
		return false
	}
	locked := policy.GetLocked()
	editable := policy.GetEditable()
	var defaultValue *publicv1.StorageTierReference
	if editable != nil {
		defaultValue = editable.GetDefaultValue()
	}
	return addReferencePricingDimension(dimensions, dimension, locked.GetName(), defaultValue.GetName(), editable != nil, defaultValue != nil)
}

func addBareMetalInstanceTypePricingDimension(dimensions map[string]string, dimension string, policy *publicv1.BareMetalInstanceTypeLocalReferenceFieldPolicy) bool {
	if policy == nil {
		return false
	}
	locked := policy.GetLocked()
	editable := policy.GetEditable()
	var defaultValue *publicv1.BareMetalInstanceTypeLocalReference
	if editable != nil {
		defaultValue = editable.GetDefaultValue()
	}
	return addReferencePricingDimension(dimensions, dimension, locked.GetName(), defaultValue.GetName(), editable != nil, defaultValue != nil)
}

func addClusterVersionPricingDimension(dimensions map[string]string, dimension string, policy *publicv1.ClusterVersionReferenceFieldPolicy) bool {
	if policy == nil {
		return false
	}
	locked := policy.GetLocked()
	editable := policy.GetEditable()
	var defaultValue *publicv1.ClusterVersionReference
	if editable != nil {
		defaultValue = editable.GetDefaultValue()
	}
	return addReferencePricingDimension(dimensions, dimension, locked.GetName(), defaultValue.GetName(), editable != nil, defaultValue != nil)
}

func addReferencePricingDimension(dimensions map[string]string, dimension, locked, defaultValue string, editable, hasDefault bool) bool {
	if locked != "" {
		dimensions[dimension] = locked
		return false
	}
	if editable && hasDefault && defaultValue != "" {
		dimensions[dimension] = defaultValue
	}
	return editable
}

func addInt32PricingDimension(dimensions map[string]string, dimension string, policy *publicv1.Int32FieldPolicy) bool {
	if policy == nil {
		return false
	}
	if policy.HasLocked() {
		if policy.GetLocked() > 0 {
			dimensions[dimension] = strconv.FormatInt(int64(policy.GetLocked()), 10)
		}
		return false
	}
	if editable := policy.GetEditable(); editable != nil {
		if editable.HasDefaultValue() && editable.GetDefaultValue() > 0 {
			dimensions[dimension] = strconv.FormatInt(int64(editable.GetDefaultValue()), 10)
		}
		return true
	}
	return false
}

func addComputeAdditionalDiskPricingDimensions(request *CatalogPricingRequest, policy *publicv1.ComputeInstanceDiskListFieldPolicy) bool {
	if policy == nil {
		return false
	}
	var disks *publicv1.ComputeInstanceDiskList
	editable := policy.GetEditable()
	if locked := policy.GetLocked(); locked != nil {
		disks = locked
	} else if editable != nil {
		disks = editable.GetDefaultValue()
	}
	if disks == nil {
		return editable != nil
	}
	if len(disks.GetItems()) > 1 {
		request.Supported = false
		return editable != nil
	}
	if len(disks.GetItems()) == 1 && disks.GetItems()[0] != nil {
		disk := disks.GetItems()[0]
		if disk.HasSizeGib() {
			request.Dimensions["additional_disk_size_gib"] = strconv.FormatInt(int64(disk.GetSizeGib()), 10)
		}
		if tier := disk.GetStorageTier(); tier != nil && tier.GetName() != "" {
			request.Dimensions["additional_storage_tier"] = tier.GetName()
		}
	}
	return editable != nil
}

func addClusterNodeSetPricingDimensions(request *CatalogPricingRequest, policy *publicv1.ClusterNodeSetMapPolicy) bool {
	if policy == nil {
		return false
	}
	var nodeSets *publicv1.ClusterNodeSetMap
	editable := policy.GetEditable()
	if locked := policy.GetLocked(); locked != nil {
		nodeSets = locked
	} else if editable != nil {
		nodeSets = editable.GetDefaultValue()
	}
	if nodeSets == nil {
		return editable != nil
	}
	items := nodeSets.GetItems()
	if len(items) > 1 {
		request.Supported = false
		return editable != nil
	}
	for _, nodeSet := range items {
		if nodeSet == nil {
			continue
		}
		if instanceType := nodeSet.GetBaremetalInstanceType(); instanceType != nil && instanceType.GetName() != "" {
			request.Dimensions["baremetal_instance_type"] = instanceType.GetName()
		}
		if nodeSet.GetSize() > 0 {
			request.Dimensions["node_count"] = strconv.FormatInt(int64(nodeSet.GetSize()), 10)
		}
	}
	return editable != nil
}

func catalogPriceDescription(title, fallback string) string {
	if title != "" {
		return title
	}
	return fallback
}
