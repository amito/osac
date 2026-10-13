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

import "context"

type RateCardSource int

const (
	RateCardSourceUnspecified RateCardSource = iota
	RateCardSourceDefault
	RateCardSourceContract
)

type RateMatchOperator int

const (
	RateMatchUnspecified RateMatchOperator = iota
	RateMatchAny
	RateMatchEquals
)

type RateUnit string

const (
	RateUnitHour  RateUnit = "hour"
	RateUnitMonth RateUnit = "month"
)

type RateCondition struct {
	Dimension string
	Operator  RateMatchOperator
	Value     string
}

type RateCardItem struct {
	Service            string
	CurrencyCode       string
	Source             RateCardSource
	Conditions         []RateCondition
	Amount             string
	Unit               RateUnit
	CatalogItemMatches []string
}

type RateCardQuery struct {
	AccountReference string
	Service          string
	CurrencyCode     string
}

// RateCardProvider reads provider pricing into OSAC's provider-neutral model.
type RateCardProvider interface {
	ListRateCardItems(context.Context, RateCardQuery) ([]RateCardItem, error)
}
