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

package billing

import (
	"errors"
	"testing"
)

func TestResolveRateCardItemPrefersMoreSpecificRule(t *testing.T) {
	rates := []RateCardItem{
		{
			Service:      "compute_instance",
			CurrencyCode: "USD",
			Amount:       "4.50",
			Unit:         RateUnitHour,
			Conditions: []RateCondition{
				{Dimension: "image_ref", Operator: RateMatchAny},
			},
		},
		{
			Service:      "compute_instance",
			CurrencyCode: "USD",
			Amount:       "3.00",
			Unit:         RateUnitHour,
			Conditions: []RateCondition{
				{Dimension: "image_ref", Operator: RateMatchEquals, Value: "fedora-16"},
				{Dimension: "boot_disk_size_gib", Operator: RateMatchEquals, Value: "20"},
			},
		},
		{
			Service:      "compute_instance",
			CurrencyCode: "EUR",
			Amount:       "2.00",
			Unit:         RateUnitHour,
		},
	}

	got, err := ResolveRateCardItem(rates, "compute_instance", "USD", map[string]string{
		"image_ref":          "fedora-16",
		"boot_disk_size_gib": "20",
	})
	if err != nil {
		t.Fatalf("ResolveRateCardItem() error = %v", err)
	}
	if got.Amount != "3.00" || got.CurrencyCode != "USD" {
		t.Fatalf("ResolveRateCardItem() = %+v, want the matching USD rate 3.00", got)
	}
}

func TestResolveRateCardItemReportsUncoveredConfiguration(t *testing.T) {
	rates := []RateCardItem{{
		Service:      "compute_instance",
		CurrencyCode: "USD",
		Amount:       "3.00",
		Unit:         RateUnitHour,
		Conditions: []RateCondition{
			{Dimension: "image_ref", Operator: RateMatchEquals, Value: "fedora-14"},
		},
	}}

	_, err := ResolveRateCardItem(rates, "compute_instance", "USD", map[string]string{"image_ref": "fedora-16"})
	if !errors.Is(err, ErrRateCoverageGap) {
		t.Fatalf("ResolveRateCardItem() error = %v, want ErrRateCoverageGap", err)
	}
}

func TestResolveRateCardItemRejectsEquallySpecificMatches(t *testing.T) {
	condition := RateCondition{Dimension: "image_ref", Operator: RateMatchEquals, Value: "fedora-16"}
	rates := []RateCardItem{
		{Service: "compute_instance", CurrencyCode: "USD", Amount: "3.00", Unit: RateUnitHour, Conditions: []RateCondition{condition}},
		{Service: "compute_instance", CurrencyCode: "USD", Amount: "4.00", Unit: RateUnitHour, Conditions: []RateCondition{condition}},
	}

	_, err := ResolveRateCardItem(rates, "compute_instance", "USD", map[string]string{"image_ref": "fedora-16"})
	if !errors.Is(err, ErrAmbiguousRateCard) {
		t.Fatalf("ResolveRateCardItem() error = %v, want ErrAmbiguousRateCard", err)
	}
}

func TestResolveRateCardItemAllowsWildcardForUnknownDimensions(t *testing.T) {
	rates := []RateCardItem{{
		Service:      "compute_instance",
		CurrencyCode: "USD",
		Amount:       "4.50",
		Unit:         RateUnitHour,
		Conditions: []RateCondition{
			{Dimension: "image_ref", Operator: RateMatchAny},
		},
	}}

	got, err := ResolveRateCardItem(rates, "compute_instance", "USD", nil)
	if err != nil {
		t.Fatalf("ResolveRateCardItem() error = %v", err)
	}
	if got.Amount != "4.50" {
		t.Fatalf("ResolveRateCardItem() amount = %q, want 4.50", got.Amount)
	}
}

func TestResolveRateCardItemRejectsInvalidRuleAndCurrency(t *testing.T) {
	tests := []struct {
		name     string
		items    []RateCardItem
		currency string
	}{
		{
			name: "missing rate amount",
			items: []RateCardItem{{
				Service: "compute_instance", CurrencyCode: "USD", Unit: RateUnitHour,
			}},
			currency: "USD",
		},
		{
			name: "unsupported match operator",
			items: []RateCardItem{{
				Service:      "compute_instance",
				CurrencyCode: "USD",
				Amount:       "4.50",
				Unit:         RateUnitHour,
				Conditions: []RateCondition{{
					Dimension: "image_ref",
					Operator:  RateMatchUnspecified,
				}},
			}},
			currency: "USD",
		},
		{
			name:     "invalid currency code",
			items:    []RateCardItem{},
			currency: "U$D",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolveRateCardItem(test.items, "compute_instance", test.currency, nil)
			if !errors.Is(err, ErrInvalidRateCard) {
				t.Fatalf("ResolveRateCardItem() error = %v, want ErrInvalidRateCard", err)
			}
		})
	}
}
