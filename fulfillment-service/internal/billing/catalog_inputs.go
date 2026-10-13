/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
*/

package billing

import (
	"sort"
	"strconv"

	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
)

const (
	ServiceComputeInstance   = "compute_instance"
	ServiceCluster           = "cluster_order"
	ServiceBareMetalInstance = "bare_metal_instance"
)

type catalogPricingInputKey struct {
	service         string
	dimension       string
	value           string
	acceptsAnyValue bool
}

type catalogPricingInputCollector struct {
	values map[catalogPricingInputKey]map[string]*publicv1.CatalogPricingInputSource
}

// CatalogPricingInputsFromCatalog projects catalog-configured values into a
// provider-independent inventory. Rows are grouped by service, dimension, and
// value or open-ended selection, with contributing catalog items retained.
func CatalogPricingInputsFromCatalog(
	computeItems []*publicv1.ComputeInstanceCatalogItem,
	clusterItems []*publicv1.ClusterCatalogItem,
	bareMetalItems []*publicv1.BareMetalInstanceCatalogItem,
) []*publicv1.CatalogPricingInput {
	collector := catalogPricingInputCollector{
		values: map[catalogPricingInputKey]map[string]*publicv1.CatalogPricingInputSource{},
	}
	collector.addComputeItems(computeItems)
	collector.addClusterItems(clusterItems)
	collector.addBareMetalItems(bareMetalItems)
	return collector.items()
}

func (c *catalogPricingInputCollector) add(service, dimension, value string, item *publicv1.CatalogPricingInputSource) {
	if value == "" || item == nil || item.GetId() == "" {
		return
	}
	key := catalogPricingInputKey{service: service, dimension: dimension, value: value}
	if c.values[key] == nil {
		c.values[key] = map[string]*publicv1.CatalogPricingInputSource{}
	}
	c.values[key][item.GetId()] = item
}

func (c *catalogPricingInputCollector) addAny(service, dimension string, item *publicv1.CatalogPricingInputSource) {
	if item == nil || item.GetId() == "" {
		return
	}
	key := catalogPricingInputKey{service: service, dimension: dimension, acceptsAnyValue: true}
	if c.values[key] == nil {
		c.values[key] = map[string]*publicv1.CatalogPricingInputSource{}
	}
	c.values[key][item.GetId()] = item
}

func (c *catalogPricingInputCollector) addPolicy(service, dimension string, values []string, acceptsAnyValue bool, item *publicv1.CatalogPricingInputSource) {
	for _, value := range values {
		c.add(service, dimension, value, item)
	}
	if acceptsAnyValue {
		c.addAny(service, dimension, item)
	}
}

func (c *catalogPricingInputCollector) addComputeItems(computeItems []*publicv1.ComputeInstanceCatalogItem) {
	for _, catalogItem := range computeItems {
		if catalogItem == nil {
			continue
		}
		source := computeCatalogSource(catalogItem)
		fields := catalogItem.GetFields()
		if fields == nil {
			continue
		}
		values, anyValue := diskImagePolicyValues(fields.GetDiskImage())
		c.addPolicy(ServiceComputeInstance, "image_ref", values, anyValue, source)
		values, anyValue = instanceTypePolicyValues(fields.GetInstanceType())
		c.addPolicy(ServiceComputeInstance, "instance_type", values, anyValue, source)
		if bootDisk := fields.GetBootDisk(); bootDisk != nil {
			values, anyValue = int32PolicyValues(bootDisk.GetSizeGib())
			c.addPolicy(ServiceComputeInstance, "boot_disk_size_gib", values, anyValue, source)
			values, anyValue = storageTierPolicyValues(bootDisk.GetStorageTier())
			c.addPolicy(ServiceComputeInstance, "storage_tier", values, anyValue, source)
		}
		disks, anyDisks := computeDiskPolicyValues(fields.GetAdditionalDisks())
		for _, disk := range disks {
			if disk.size != nil {
				c.add(ServiceComputeInstance, "additional_disk_size_gib", strconv.FormatInt(int64(*disk.size), 10), source)
			}
			c.add(ServiceComputeInstance, "additional_storage_tier", disk.storageTier, source)
		}
		if anyDisks {
			c.addAny(ServiceComputeInstance, "additional_disk_size_gib", source)
			c.addAny(ServiceComputeInstance, "additional_storage_tier", source)
		}
	}
}

func (c *catalogPricingInputCollector) addClusterItems(clusterItems []*publicv1.ClusterCatalogItem) {
	for _, catalogItem := range clusterItems {
		if catalogItem == nil {
			continue
		}
		source := clusterCatalogSource(catalogItem)
		if template := catalogItem.GetTemplate(); template != nil {
			c.add(ServiceCluster, "cluster_template", template.GetName(), source)
		}
		fields := catalogItem.GetFields()
		if fields == nil {
			continue
		}
		values, anyValue := clusterVersionPolicyValues(fields.GetVersion())
		c.addPolicy(ServiceCluster, "release_image", values, anyValue, source)
		nodeSets, anyNodeSet := clusterNodeSetPolicyValues(fields.GetNodeSets())
		for _, nodeSet := range nodeSets {
			c.add(ServiceCluster, "baremetal_instance_type", nodeSet.instanceType, source)
			if nodeSet.size > 0 {
				c.add(ServiceCluster, "node_count", strconv.FormatInt(int64(nodeSet.size), 10), source)
			}
		}
		if anyNodeSet {
			c.addAny(ServiceCluster, "baremetal_instance_type", source)
			c.addAny(ServiceCluster, "node_count", source)
		}
	}
}

func (c *catalogPricingInputCollector) addBareMetalItems(bareMetalItems []*publicv1.BareMetalInstanceCatalogItem) {
	for _, catalogItem := range bareMetalItems {
		if catalogItem == nil {
			continue
		}
		source := bareMetalCatalogSource(catalogItem)
		c.add(ServiceBareMetalInstance, "catalog_item", source.GetName(), source)
		fields := catalogItem.GetFields()
		if fields == nil {
			continue
		}
		values, anyValue := bareMetalInstanceTypePolicyValues(fields.GetInstanceType())
		c.addPolicy(ServiceBareMetalInstance, "bm_instance_type", values, anyValue, source)
		values, anyValue = diskImagePolicyValues(fields.GetDiskImage())
		c.addPolicy(ServiceBareMetalInstance, "image_ref", values, anyValue, source)
	}
}

func (c *catalogPricingInputCollector) items() []*publicv1.CatalogPricingInput {
	keys := make([]catalogPricingInputKey, 0, len(c.values))
	for key := range c.values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].service != keys[j].service {
			return keys[i].service < keys[j].service
		}
		if keys[i].dimension != keys[j].dimension {
			return keys[i].dimension < keys[j].dimension
		}
		if keys[i].acceptsAnyValue != keys[j].acceptsAnyValue {
			return !keys[i].acceptsAnyValue
		}
		return keys[i].value < keys[j].value
	})

	result := make([]*publicv1.CatalogPricingInput, 0, len(keys))
	for _, key := range keys {
		sources := make([]*publicv1.CatalogPricingInputSource, 0, len(c.values[key]))
		for _, source := range c.values[key] {
			sources = append(sources, source)
		}
		sort.Slice(sources, func(i, j int) bool {
			if sources[i].GetName() != sources[j].GetName() {
				return sources[i].GetName() < sources[j].GetName()
			}
			return sources[i].GetId() < sources[j].GetId()
		})
		result = append(result, publicv1.CatalogPricingInput_builder{
			Service:         key.service,
			Dimension:       key.dimension,
			Value:           key.value,
			CatalogItems:    sources,
			AcceptsAnyValue: key.acceptsAnyValue,
		}.Build())
	}
	return result
}

func computeCatalogSource(item *publicv1.ComputeInstanceCatalogItem) *publicv1.CatalogPricingInputSource {
	return catalogPricingSource(item.GetId(), item.GetMetadata().GetName(), item.GetTitle(), item.GetPublished())
}

func clusterCatalogSource(item *publicv1.ClusterCatalogItem) *publicv1.CatalogPricingInputSource {
	return catalogPricingSource(item.GetId(), item.GetMetadata().GetName(), item.GetTitle(), item.GetPublished())
}

func bareMetalCatalogSource(item *publicv1.BareMetalInstanceCatalogItem) *publicv1.CatalogPricingInputSource {
	return catalogPricingSource(item.GetId(), item.GetMetadata().GetName(), item.GetTitle(), item.GetPublished())
}

func catalogPricingSource(id, name, title string, published bool) *publicv1.CatalogPricingInputSource {
	return publicv1.CatalogPricingInputSource_builder{Id: id, Name: name, Title: title, Published: published}.Build()
}

func diskImagePolicyValues(policy *publicv1.DiskImageReferenceFieldPolicy) ([]string, bool) {
	if policy == nil {
		return nil, false
	}
	if ref := policy.GetLocked(); ref != nil {
		return nonEmptyPolicyValue(ref.GetName()), false
	}
	if editable := policy.GetEditable(); editable != nil {
		if editable.GetDefaultValue() != nil {
			return nonEmptyPolicyValue(editable.GetDefaultValue().GetName()), true
		}
		return nil, true
	}
	return nil, false
}

func instanceTypePolicyValues(policy *publicv1.InstanceTypeReferenceFieldPolicy) ([]string, bool) {
	if policy == nil {
		return nil, false
	}
	if ref := policy.GetLocked(); ref != nil {
		return nonEmptyPolicyValue(ref.GetName()), false
	}
	if editable := policy.GetEditable(); editable != nil {
		if editable.GetDefaultValue() != nil {
			return nonEmptyPolicyValue(editable.GetDefaultValue().GetName()), true
		}
		return nil, true
	}
	return nil, false
}

func storageTierPolicyValues(policy *publicv1.StorageTierReferenceFieldPolicy) ([]string, bool) {
	if policy == nil {
		return nil, false
	}
	if ref := policy.GetLocked(); ref != nil {
		return nonEmptyPolicyValue(ref.GetName()), false
	}
	if editable := policy.GetEditable(); editable != nil {
		if editable.GetDefaultValue() != nil {
			return nonEmptyPolicyValue(editable.GetDefaultValue().GetName()), true
		}
		return nil, true
	}
	return nil, false
}

func clusterVersionPolicyValues(policy *publicv1.ClusterVersionReferenceFieldPolicy) ([]string, bool) {
	if policy == nil {
		return nil, false
	}
	if ref := policy.GetLocked(); ref != nil {
		return nonEmptyPolicyValue(ref.GetName()), false
	}
	if editable := policy.GetEditable(); editable != nil {
		if editable.GetDefaultValue() != nil {
			return nonEmptyPolicyValue(editable.GetDefaultValue().GetName()), true
		}
		return nil, true
	}
	return nil, false
}

func bareMetalInstanceTypePolicyValues(policy *publicv1.BareMetalInstanceTypeLocalReferenceFieldPolicy) ([]string, bool) {
	if policy == nil {
		return nil, false
	}
	if ref := policy.GetLocked(); ref != nil {
		return nonEmptyPolicyValue(ref.GetName()), false
	}
	if editable := policy.GetEditable(); editable != nil {
		if editable.GetDefaultValue() != nil {
			return nonEmptyPolicyValue(editable.GetDefaultValue().GetName()), true
		}
		return nil, true
	}
	return nil, false
}

func int32PolicyValues(policy *publicv1.Int32FieldPolicy) ([]string, bool) {
	if policy == nil {
		return nil, false
	}
	if policy.HasLocked() {
		if policy.GetLocked() > 0 {
			return []string{strconv.FormatInt(int64(policy.GetLocked()), 10)}, false
		}
		return nil, false
	}
	if editable := policy.GetEditable(); editable != nil {
		if editable.HasDefaultValue() && editable.GetDefaultValue() > 0 {
			return []string{strconv.FormatInt(int64(editable.GetDefaultValue()), 10)}, true
		}
		return nil, true
	}
	return nil, false
}

func nonEmptyPolicyValue(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}

type computeCatalogDisk struct {
	size        *int32
	storageTier string
}

func computeDiskPolicyValues(policy *publicv1.ComputeInstanceDiskListFieldPolicy) ([]computeCatalogDisk, bool) {
	if policy == nil {
		return nil, false
	}
	var disks *publicv1.ComputeInstanceDiskList
	acceptsAny := false
	if locked := policy.GetLocked(); locked != nil {
		disks = locked
	} else if editable := policy.GetEditable(); editable != nil {
		disks = editable.GetDefaultValue()
		acceptsAny = true
	}
	if disks == nil {
		return nil, acceptsAny
	}
	result := make([]computeCatalogDisk, 0, len(disks.GetItems()))
	for _, disk := range disks.GetItems() {
		if disk == nil {
			continue
		}
		var size *int32
		if disk.HasSizeGib() {
			value := disk.GetSizeGib()
			size = &value
		}
		result = append(result, computeCatalogDisk{size: size, storageTier: disk.GetStorageTier().GetName()})
	}
	return result, acceptsAny
}

type clusterCatalogNodeSet struct {
	instanceType string
	size         int32
}

func clusterNodeSetPolicyValues(policy *publicv1.ClusterNodeSetMapPolicy) ([]clusterCatalogNodeSet, bool) {
	if policy == nil {
		return nil, false
	}
	var nodeSets *publicv1.ClusterNodeSetMap
	acceptsAny := false
	if locked := policy.GetLocked(); locked != nil {
		nodeSets = locked
	} else if editable := policy.GetEditable(); editable != nil {
		nodeSets = editable.GetDefaultValue()
		acceptsAny = true
	}
	if nodeSets == nil {
		return nil, acceptsAny
	}
	keys := make([]string, 0, len(nodeSets.GetItems()))
	for key := range nodeSets.GetItems() {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]clusterCatalogNodeSet, 0, len(keys))
	for _, key := range keys {
		nodeSet := nodeSets.GetItems()[key]
		if nodeSet == nil {
			continue
		}
		result = append(result, clusterCatalogNodeSet{
			instanceType: nodeSet.GetBaremetalInstanceType().GetName(),
			size:         nodeSet.GetSize(),
		})
	}
	return result, acceptsAny
}
