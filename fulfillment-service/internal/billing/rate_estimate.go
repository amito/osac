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
	"math/big"
	"strings"
)

const (
	hoursPerBillingMonth     = 730
	derivedRateDecimalPlaces = 6
)

// NormalizeRecurringRate returns hourly and monthly equivalents for provider
// hourly or monthly rates. Monthly-to-hourly conversion is represented to six
// decimal places; the provider amount and unit remain unchanged in the API.
func NormalizeRecurringRate(amount string, unit RateUnit) (hourly, monthly string, err error) {
	value, ok := new(big.Rat).SetString(strings.TrimSpace(amount))
	if !ok || value.Sign() < 0 {
		return "", "", errors.New("rate amount must be a non-negative decimal")
	}
	month := big.NewRat(hoursPerBillingMonth, 1)
	switch unit {
	case RateUnitHour:
		return formatDerivedRate(value), formatDerivedRate(new(big.Rat).Mul(value, month)), nil
	case RateUnitMonth:
		return formatDerivedRate(new(big.Rat).Quo(value, month)), formatDerivedRate(value), nil
	default:
		return "", "", errors.New("rate unit is not a supported recurring time unit")
	}
}

func formatDerivedRate(value *big.Rat) string {
	formatted := value.FloatString(derivedRateDecimalPlaces)
	formatted = strings.TrimRight(formatted, "0")
	return strings.TrimRight(formatted, ".")
}
