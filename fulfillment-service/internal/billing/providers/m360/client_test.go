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

package m360

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/osac-project/osac/fulfillment-service/internal/billing"
)

var _ = Describe("M360 billing account client", func() {
	It("reports the provider as available when the account API responds successfully", func() {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			Expect(request.Method).To(Equal(http.MethodGet))
			Expect(request.URL.Path).To(Equal("/api/v1/external/accounts"))
			Expect(request.URL.Query().Get("size")).To(Equal("1"))
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(writer, `{"success":true,"status":200,"data":{"content":[],"totalElements":0,"totalPages":0,"size":1,"number":0,"last":true}}`)
		}))
		DeferCleanup(server.Close)

		client, err := NewClient(Config{
			BaseURL:    server.URL,
			APIKey:     "test-token",
			APIVersion: "v1",
			HTTPClient: server.Client(),
		})
		Expect(err).NotTo(HaveOccurred())

		status, err := client.GetStatus(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(status.State).To(Equal(billing.ProviderStateAvailable))
		Expect(status.Capabilities).To(Equal([]billing.ProviderCapability{
			billing.ProviderCapabilityPricing,
			billing.ProviderCapabilityCosts,
			billing.ProviderCapabilityInvoices,
		}))
	})

	It("retrieves one account by its provider identifier", func() {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			Expect(request.Method).To(Equal(http.MethodGet))
			Expect(request.URL.Path).To(Equal("/api/v1/external/accounts/account-7"))
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(writer, `{"success":true,"status":200,"data":{"Id":"account-7","Name":"Acme","ExternalId":"tenant-a","Status__r":{"Name":"Active"}}}`)
		}))
		DeferCleanup(server.Close)

		client, err := NewClient(Config{
			BaseURL:    server.URL,
			APIKey:     "test-token",
			APIVersion: "v1",
			HTTPClient: server.Client(),
		})
		Expect(err).NotTo(HaveOccurred())

		account, err := client.GetAccount(context.Background(), "account-7")
		Expect(err).NotTo(HaveOccurred())
		Expect(account).To(Equal(billing.Account{
			Reference:   "account-7",
			DisplayName: "Acme",
			ExternalID:  "tenant-a",
			State:       billing.AccountStateActive,
		}))
	})

	It("updates only the external ID scalar field on an account", func() {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			Expect(request.URL.Path).To(Equal("/api/v1/external/accounts/account-7"))
			Expect(request.Header.Get("Authorization")).To(Equal("Bearer test-token"))
			if request.Method == http.MethodGet {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(writer, `{"success":true,"status":200,"data":{"Id":"account-7","Name":"Acme","ExternalId":"tenant-old"}}`)
				return
			}
			Expect(request.Method).To(Equal(http.MethodPut))
			body := make(map[string]any)
			Expect(json.NewDecoder(request.Body).Decode(&body)).To(Succeed())
			Expect(body).To(Equal(map[string]any{"externalId": "tenant-a"}))
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(writer, `{"id":"account-7","name":"Acme","externalId":"tenant-a"}`)
		}))
		DeferCleanup(server.Close)

		client, err := NewClient(Config{
			BaseURL:    server.URL,
			APIKey:     "test-token",
			APIVersion: "v1",
			HTTPClient: server.Client(),
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(client.UpdateAccountExternalID(context.Background(), "account-7", "tenant-old", "tenant-a")).To(Succeed())
	})

	It("does not update an account whose external ID changed since it was read", func() {
		putRequested := false
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodPut {
				putRequested = true
				return
			}
			Expect(request.Method).To(Equal(http.MethodGet))
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(writer, `{"success":true,"status":200,"data":{"Id":"account-7","Name":"Acme","ExternalId":"tenant-b"}}`)
		}))
		DeferCleanup(server.Close)

		client, err := NewClient(Config{
			BaseURL:    server.URL,
			APIKey:     "test-token",
			APIVersion: "v1",
			HTTPClient: server.Client(),
		})
		Expect(err).NotTo(HaveOccurred())

		err = client.UpdateAccountExternalID(context.Background(), "account-7", "tenant-a", "tenant-c")
		Expect(errors.Is(err, billing.ErrAccountExternalIDChanged)).To(BeTrue())
		Expect(putRequested).To(BeFalse())
	})

	It("lists all accounts across provider pages and normalizes their status", func() {
		var requestedPages []string
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			Expect(request.Method).To(Equal(http.MethodGet))
			Expect(request.URL.Path).To(Equal("/api/v1/external/accounts"))
			Expect(request.Header.Get("Authorization")).To(Equal("Bearer test-token"))
			Expect(request.URL.Query().Get("size")).To(Equal("20"))
			requestedPages = append(requestedPages, request.URL.Query().Get("page"))

			page := request.URL.Query().Get("page")
			writer.Header().Set("Content-Type", "application/json")
			if page == "0" {
				_, _ = fmt.Fprint(writer, `{"success":true,"status":200,"data":{"content":[{"Id":"account-1","Name":"Acme","ExternalId":"tenant-a","Status__r":{"Name":"Active"}}],"totalElements":2,"totalPages":2,"size":20,"number":0,"last":false}}`)
				return
			}
			_, _ = fmt.Fprint(writer, `{"success":true,"status":200,"data":{"content":[{"Id":"account-2","Name":"Northwind","ExternalId":"tenant-b","Status__r":{"Name":"Inactive"}}],"totalElements":2,"totalPages":2,"size":20,"number":1,"last":true}}`)
		}))
		DeferCleanup(server.Close)

		client, err := NewClient(Config{
			BaseURL:    server.URL,
			APIKey:     "test-token",
			APIVersion: "v1",
			HTTPClient: server.Client(),
		})
		Expect(err).NotTo(HaveOccurred())

		accounts, err := client.ListAccounts(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(requestedPages).To(Equal([]string{"0", "1"}))
		Expect(accounts).To(Equal([]billing.Account{
			{Reference: "account-1", DisplayName: "Acme", ExternalID: "tenant-a", State: billing.AccountStateActive},
			{Reference: "account-2", DisplayName: "Northwind", ExternalID: "tenant-b", State: billing.AccountStateInactive},
		}))
	})

	It("lists provider-neutral rows from an OSAC-matched M360 service rate card", func() {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			switch request.URL.Path {
			case "/api/v1/external/services":
				Expect(request.Method).To(Equal(http.MethodGet))
				Expect(request.URL.Query().Get("page")).To(Equal("0"))
				_, _ = fmt.Fprint(writer, `{"success":true,"status":200,"data":{"content":[{"Id":"service-1","Name":"Compute","Code":"compute_instance"}],"totalElements":1,"totalPages":1,"last":true}}`)
			case "/api/v1/external/services/service-1":
				Expect(request.Method).To(Equal(http.MethodGet))
				_, _ = fmt.Fprint(writer, `{"success":true,"status":200,"data":{"id":"service-1","name":"Compute","code":"compute_instance","rateCards":[{"currency":"USD","isCurrent":true,"rows":[{"priceDimId":"dimension-1","priceMethod":"FLAT_RATE","flatRate":4.50,"attributeValues":[{"attributeName":"vm_image_ref","attributeValue":"fedora-16"}]}]}]}}`)
			default:
				Fail("unexpected M360 request path: " + request.URL.Path)
			}
		}))
		DeferCleanup(server.Close)

		client, err := NewClient(Config{BaseURL: server.URL, APIKey: "test-token", APIVersion: "v1", HTTPClient: server.Client()})
		Expect(err).NotTo(HaveOccurred())

		items, err := client.ListRateCardItems(context.Background(), billing.RateCardQuery{
			Service:      "compute_instance",
			CurrencyCode: "USD",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(items).To(Equal([]billing.RateCardItem{{
			Service:      "compute_instance",
			CurrencyCode: "USD",
			Source:       billing.RateCardSourceDefault,
			Conditions: []billing.RateCondition{{
				Dimension: "image_ref",
				Operator:  billing.RateMatchEquals,
				Value:     "fedora-16",
			}},
			Amount: "4.50",
			Unit:   billing.RateUnitMonth,
		}}))
	})

	It("uses the active account contract's service rate card when an exception exists", func() {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			switch request.URL.Path {
			case "/api/v1/external/services":
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"content":[{"Id":"service-1","Code":"compute_instance"}],"totalPages":1,"last":true}}`)
			case "/api/v1/external/services/service-1":
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"id":"service-1","code":"compute_instance","rateCards":[{"currency":"USD","isCurrent":true,"rows":[{"priceMethod":"FLAT_RATE","flatRate":99.0,"attributeValues":[] }]}]}}`)
			case "/api/v1/external/contracts":
				Expect(request.URL.Query().Get("page")).To(Equal("0"))
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"content":[{"Id":"contract-1","MasterAccountId":"account-1","ApprovalStatus__r":{"Name":"Approved"},"StartDate":"2000-01-01"}],"totalPages":1,"last":true}}`)
			case "/api/v1/external/contracts/contract-1/services":
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"content":[{"id":"contract-service-1","serviceId":"service-1","startDate":"2000-01-01","exceptionPricing":true}],"totalPages":1,"last":true}}`)
			case "/api/v1/external/contracts/contract-1/services/contract-service-1/rate-cards/USD":
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"currency":"USD","rows":[{"priceMethod":"QUANTITY","unitRate":2.75,"attributeValues":[{"attributeName":"vm_image_ref","attributeValue":"fedora-16"}]}]}}`)
			default:
				Fail("unexpected M360 request path: " + request.URL.Path)
			}
		}))
		DeferCleanup(server.Close)

		client, err := NewClient(Config{BaseURL: server.URL, APIKey: "test-token", APIVersion: "v1", HTTPClient: server.Client()})
		Expect(err).NotTo(HaveOccurred())

		items, err := client.ListRateCardItems(context.Background(), billing.RateCardQuery{
			AccountReference: "account-1",
			Service:          "compute_instance",
			CurrencyCode:     "USD",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(items).To(Equal([]billing.RateCardItem{{
			Service:      "compute_instance",
			CurrencyCode: "USD",
			Source:       billing.RateCardSourceContract,
			Conditions: []billing.RateCondition{{
				Dimension: "image_ref",
				Operator:  billing.RateMatchEquals,
				Value:     "fedora-16",
			}},
			Amount: "2.75",
			Unit:   billing.RateUnitHour,
		}}))
	})

	It("does not fall back to a default rate card when the account contract is ambiguous", func() {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			switch request.URL.Path {
			case "/api/v1/external/services":
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"content":[{"Id":"service-1","Code":"compute_instance"}],"totalPages":1,"last":true}}`)
			case "/api/v1/external/contracts":
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"content":[{"Id":"contract-1","MasterAccountId":"account-1","ApprovalStatus__r":{"Name":"Approved"}},{"Id":"contract-2","MasterAccountId":"account-1","ApprovalStatus__r":{"Name":"Approved"}}],"totalPages":1,"last":true}}`)
			default:
				Fail("unexpected M360 request path: " + request.URL.Path)
			}
		}))
		DeferCleanup(server.Close)

		client, err := NewClient(Config{BaseURL: server.URL, APIKey: "test-token", APIVersion: "v1", HTTPClient: server.Client()})
		Expect(err).NotTo(HaveOccurred())

		_, err = client.ListRateCardItems(context.Background(), billing.RateCardQuery{
			AccountReference: "account-1",
			Service:          "compute_instance",
			CurrencyCode:     "USD",
		})
		Expect(err).To(MatchError(ContainSubstring("multiple applicable account contracts")))
	})

	It("uses the service catalog card when the applicable contract has no service exception", func() {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			switch request.URL.Path {
			case "/api/v1/external/services":
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"content":[{"Id":"service-1","Code":"compute_instance"}],"totalPages":1,"last":true}}`)
			case "/api/v1/external/contracts":
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"content":[{"Id":"contract-1","MasterAccountId":"account-1","ApprovalStatus__r":{"Name":"Approved"},"StartDate":"2000-01-01"}],"totalPages":1,"last":true}}`)
			case "/api/v1/external/services/service-1":
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"id":"service-1","code":"compute_instance","rateCards":[{"currency":"USD","isCurrent":true,"rows":[{"priceMethod":"QUANTITY","unitRate":4.5,"attributeValues":[]}]}]}}`)
			case "/api/v1/external/contracts/contract-1/services":
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"content":[{"id":"contract-service-1","serviceId":"service-1","startDate":"2000-01-01","exceptionPricing":false}],"totalPages":1,"last":true}}`)
			default:
				Fail("unexpected M360 request path: " + request.URL.Path)
			}
		}))
		DeferCleanup(server.Close)

		client, err := NewClient(Config{BaseURL: server.URL, APIKey: "test-token", APIVersion: "v1", HTTPClient: server.Client()})
		Expect(err).NotTo(HaveOccurred())

		items, err := client.ListRateCardItems(context.Background(), billing.RateCardQuery{
			AccountReference: "account-1",
			Service:          "compute_instance",
			CurrencyCode:     "USD",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(items).To(HaveLen(1))
		Expect(items[0].Source).To(Equal(billing.RateCardSourceContract))
		Expect(items[0].Amount).To(Equal("4.5"))
	})

	It("does not use the catalog card when an exception-priced contract rate card is missing", func() {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			switch request.URL.Path {
			case "/api/v1/external/services":
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"content":[{"Id":"service-1","Code":"compute_instance"}],"totalPages":1,"last":true}}`)
			case "/api/v1/external/services/service-1":
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"id":"service-1","code":"compute_instance","rateCards":[{"currency":"USD","isCurrent":true,"rows":[{"priceMethod":"FLAT_RATE","flatRate":99.0,"attributeValues":[]}] }]}}`)
			case "/api/v1/external/contracts":
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"content":[{"Id":"contract-1","MasterAccountId":"account-1","ApprovalStatus__r":{"Name":"Approved"},"StartDate":"2000-01-01"}],"totalPages":1,"last":true}}`)
			case "/api/v1/external/contracts/contract-1/services":
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"content":[{"id":"contract-service-1","serviceId":"service-1","startDate":"2000-01-01","exceptionPricing":true}],"totalPages":1,"last":true}}`)
			case "/api/v1/external/contracts/contract-1/services/contract-service-1/rate-cards/USD":
				writer.WriteHeader(http.StatusNotFound)
			default:
				Fail("unexpected M360 request path: " + request.URL.Path)
			}
		}))
		DeferCleanup(server.Close)

		client, err := NewClient(Config{BaseURL: server.URL, APIKey: "test-token", APIVersion: "v1", HTTPClient: server.Client()})
		Expect(err).NotTo(HaveOccurred())

		items, err := client.ListRateCardItems(context.Background(), billing.RateCardQuery{
			AccountReference: "account-1",
			Service:          "compute_instance",
			CurrencyCode:     "USD",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(items).To(BeEmpty())
	})

	It("maps M360 VM attribute names to OSAC billing dimensions", func() {
		item, err := normalizeRateCardItem("compute_instance", "USD", billing.RateCardSourceDefault, m360RateCardRow{
			PriceMethod: "FLAT_RATE",
			FlatRate:    json.Number("4.50"),
			AttributeValues: []m360RateAttribute{
				{AttributeName: "vm_image_ref", AttributeValue: "fedora-16"},
				{AttributeName: "boot_disk_size", AttributeValue: "*"},
			},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(item.Conditions).To(Equal([]billing.RateCondition{
			{Dimension: "image_ref", Operator: billing.RateMatchEquals, Value: "fedora-16"},
			{Dimension: "boot_disk_size_gib", Operator: billing.RateMatchAny},
		}))
	})

	It("does not include the provider response body in errors", func() {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(writer, `{"message":"sensitive account data"}`)
		}))
		DeferCleanup(server.Close)

		client, err := NewClient(Config{
			BaseURL:    server.URL,
			APIKey:     "test-token",
			APIVersion: "v1",
			HTTPClient: server.Client(),
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = client.ListAccounts(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).NotTo(ContainSubstring("sensitive account data"))
	})
})
