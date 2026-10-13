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
	"context"
	"errors"
)

var ErrAccountExternalIDChanged = errors.New("billing account external ID changed before update")

// AccountState is OSAC's normalized account state, independent of a billing
// provider's status vocabulary.
type AccountState int

const (
	AccountStateUnknown AccountState = iota
	AccountStateActive
	AccountStateInactive
)

// Account is the provider-neutral subset of a billing account used by OSAC.
type Account struct {
	Reference   string
	DisplayName string
	ExternalID  string
	Currency    string
	State       AccountState
}

// ProviderState is OSAC's normalized connectivity state for a provider.
type ProviderState int

const (
	ProviderStateUnknown ProviderState = iota
	ProviderStateNotConfigured
	ProviderStateAvailable
	ProviderStateUnavailable
)

// ProviderStatus contains normalized provider health information.
type ProviderStatus struct {
	State        ProviderState
	ProviderKey  string
	Message      string
	Capabilities []ProviderCapability
}

// ProviderCapability identifies a billing operation implemented by the
// configured provider adapter.
type ProviderCapability string

const (
	ProviderCapabilityPricing     ProviderCapability = "PRICING"
	ProviderCapabilityCosts       ProviderCapability = "COSTS"
	ProviderCapabilityInvoices    ProviderCapability = "INVOICES"
	ProviderCapabilityDraftCharge ProviderCapability = "DRAFT_CHARGES"
	ProviderCapabilityExports     ProviderCapability = "EXPORTS"
)

// Provider exposes the billing operations required by the first account-link
// slice. Implementations translate provider-native formats into these types.
type Provider interface {
	GetStatus(context.Context) (ProviderStatus, error)
	ListAccounts(context.Context) ([]Account, error)
	GetAccount(context.Context, string) (Account, error)
	UpdateAccountExternalID(ctx context.Context, reference, expectedExternalID, newExternalID string) error
}

// ResolveProviderStatus returns a safe, provider-neutral status for the
// deployment. Provider error text is deliberately not returned to clients.
func ResolveProviderStatus(ctx context.Context, providerKey string, provider Provider) ProviderStatus {
	if provider == nil {
		return ProviderStatus{State: ProviderStateNotConfigured}
	}
	status, err := provider.GetStatus(ctx)
	status.ProviderKey = providerKey
	if err != nil {
		return ProviderStatus{
			State:       ProviderStateUnavailable,
			ProviderKey: providerKey,
			Message:     "provider health check failed",
		}
	}
	return status
}
