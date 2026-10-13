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

package billing

import (
	"errors"
	"strconv"
	"strings"

	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
)

// CatalogPricingRequest describes one resource configuration to resolve.
type CatalogPricingRequest struct {
	Service      string
	Dimensions   map[string]string
	Description  string
	PriceMayVary bool
	Supported    bool
}

var ErrInvalidPricingConfiguration = errors.New("pricing configuration is invalid for the catalog item")

// ApplyComputeInstancePricingConfiguration overlays the billable values chosen
// for a compute instance on the catalog item's locked and default values.
func ApplyComputeInstancePricingConfiguration(
	request CatalogPricingRequest,
	spec *publicv1.ComputeInstanceSpec,
	fields *publicv1.ComputeInstanceCatalogItemFields,
) (CatalogPricingRequest, error) {
	if spec == nil || fields == nil {
		return request, ErrInvalidPricingConfiguration
	}
	if request.Dimensions == nil {
		request.Dimensions = make(map[string]string)
	}
	if err := applyReferencePricingSelection(request.Dimensions, "image_ref", spec.GetDiskImage().GetName(), fields.GetDiskImage().GetLocked().GetName(), fields.GetDiskImage().GetEditable() != nil); err != nil {
		return request, err
	}
	if err := applyReferencePricingSelection(request.Dimensions, "instance_type", spec.GetInstanceType().GetName(), fields.GetInstanceType().GetLocked().GetName(), fields.GetInstanceType().GetEditable() != nil); err != nil {
		return request, err
	}
	if disk := spec.GetBootDisk(); disk != nil {
		policy := fields.GetBootDisk()
		if disk.HasSizeGib() {
			lockedSize := policy.GetSizeGib().GetLocked()
			selectedSize := disk.GetSizeGib()
			if policy.GetSizeGib().HasLocked() && lockedSize != selectedSize {
				return request, ErrInvalidPricingConfiguration
			}
			if !policy.GetSizeGib().HasLocked() && policy.GetSizeGib().GetEditable() == nil {
				return request, ErrInvalidPricingConfiguration
			}
			request.Dimensions["boot_disk_size_gib"] = strconv.FormatInt(int64(selectedSize), 10)
		}
		if tier := disk.GetStorageTier().GetName(); tier != "" {
			if err := applyReferencePricingSelection(request.Dimensions, "storage_tier", tier, policy.GetStorageTier().GetLocked().GetName(), policy.GetStorageTier().GetEditable() != nil); err != nil {
				return request, err
			}
		}
	}
	additionalDisks := spec.GetAdditionalDisks()
	if len(additionalDisks) > 1 {
		request.Supported = false
		return request, nil
	}
	if len(additionalDisks) == 1 && additionalDisks[0] != nil {
		policy := fields.GetAdditionalDisks()
		if policy == nil {
			return request, ErrInvalidPricingConfiguration
		}
		if locked := policy.GetLocked(); locked != nil {
			items := locked.GetItems()
			if len(items) != 1 || items[0].GetSizeGib() != additionalDisks[0].GetSizeGib() || items[0].GetStorageTier().GetName() != additionalDisks[0].GetStorageTier().GetName() {
				return request, ErrInvalidPricingConfiguration
			}
		} else if policy.GetEditable() == nil {
			return request, ErrInvalidPricingConfiguration
		}
		if additionalDisks[0].HasSizeGib() {
			request.Dimensions["additional_disk_size_gib"] = strconv.FormatInt(int64(additionalDisks[0].GetSizeGib()), 10)
		}
		if tier := additionalDisks[0].GetStorageTier().GetName(); tier != "" {
			request.Dimensions["additional_storage_tier"] = tier
		}
	}
	return request, nil
}

// ApplyClusterPricingConfiguration overlays the selected cluster version and
// node set on the catalog item's locked and default values.
func ApplyClusterPricingConfiguration(
	request CatalogPricingRequest,
	spec *publicv1.ClusterSpec,
	fields *publicv1.ClusterCatalogItemFields,
) (CatalogPricingRequest, error) {
	if spec == nil || fields == nil {
		return request, ErrInvalidPricingConfiguration
	}
	if request.Dimensions == nil {
		request.Dimensions = make(map[string]string)
	}
	if err := applyReferencePricingSelection(request.Dimensions, "release_image", spec.GetVersion().GetName(), fields.GetVersion().GetLocked().GetName(), fields.GetVersion().GetEditable() != nil); err != nil {
		return request, err
	}
	nodeSets := spec.GetNodeSets()
	if len(nodeSets) > 1 {
		request.Supported = false
		return request, nil
	}
	for _, nodeSet := range nodeSets {
		if nodeSet == nil {
			continue
		}
		if err := validateClusterNodeSetSelection(fields.GetNodeSets(), nodeSet); err != nil {
			return request, err
		}
		if instanceType := nodeSet.GetBaremetalInstanceType().GetName(); instanceType != "" {
			request.Dimensions["baremetal_instance_type"] = instanceType
		}
		if nodeSet.HasSize() {
			request.Dimensions["node_count"] = strconv.FormatInt(int64(nodeSet.GetSize()), 10)
		}
	}
	return request, nil
}

// ApplyBareMetalPricingConfiguration overlays the selected bare-metal type and
// image on the catalog item's locked and default values.
func ApplyBareMetalPricingConfiguration(
	request CatalogPricingRequest,
	spec *publicv1.BareMetalInstanceSpec,
	fields *publicv1.BareMetalInstanceCatalogItemFields,
) (CatalogPricingRequest, error) {
	if spec == nil || fields == nil {
		return request, ErrInvalidPricingConfiguration
	}
	if request.Dimensions == nil {
		request.Dimensions = make(map[string]string)
	}
	if err := applyReferencePricingSelection(request.Dimensions, "bm_instance_type", spec.GetInstanceType().GetName(), fields.GetInstanceType().GetLocked().GetName(), fields.GetInstanceType().GetEditable() != nil); err != nil {
		return request, err
	}
	if err := applyReferencePricingSelection(request.Dimensions, "image_ref", spec.GetDiskImage().GetName(), fields.GetDiskImage().GetLocked().GetName(), fields.GetDiskImage().GetEditable() != nil); err != nil {
		return request, err
	}
	return request, nil
}

func applyReferencePricingSelection(dimensions map[string]string, dimension, selected, locked string, editable bool) error {
	selected = strings.TrimSpace(selected)
	locked = strings.TrimSpace(locked)
	if selected == "" {
		return nil
	}
	if locked != "" {
		if selected != locked {
			return ErrInvalidPricingConfiguration
		}
		return nil
	}
	if !editable {
		return ErrInvalidPricingConfiguration
	}
	dimensions[dimension] = selected
	return nil
}

func validateClusterNodeSetSelection(policy *publicv1.ClusterNodeSetMapPolicy, selected *publicv1.ClusterNodeSet) error {
	if policy == nil || selected == nil {
		return ErrInvalidPricingConfiguration
	}
	if locked := policy.GetLocked(); locked != nil {
		if len(locked.GetItems()) != 1 {
			return ErrInvalidPricingConfiguration
		}
		for _, value := range locked.GetItems() {
			if value.GetSize() != selected.GetSize() || value.GetBaremetalInstanceType().GetName() != selected.GetBaremetalInstanceType().GetName() {
				return ErrInvalidPricingConfiguration
			}
		}
		return nil
	}
	if policy.GetEditable() == nil {
		return ErrInvalidPricingConfiguration
	}
	return nil
}
