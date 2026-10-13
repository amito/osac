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
	"fmt"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/osac-project/osac/fulfillment-service/internal/billing"
)

var _ = Describe("M360 actual cost reader", func() {
	It("aggregates rated transactions by OSAC service, project, and resource", func() {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			switch request.URL.Path {
			case "/api/v1/external/services":
				Expect(request.URL.Query().Get("page")).To(Equal("0"))
				_, _ = fmt.Fprint(writer, `{"success":true,"status":200,"data":{"content":[{"Id":"service-1","Code":"compute_instance"}],"totalElements":1,"totalPages":1,"size":20,"number":0,"last":true}}`)
			case "/api/v1/external/rated-transactions":
				Expect(request.URL.Query().Get("accountId")).To(Equal("account-1"))
				Expect(request.URL.Query().Get("fromDate")).To(Equal("2026-08-01"))
				Expect(request.URL.Query().Get("toDate")).To(Equal("2026-08-31"))
				_, _ = fmt.Fprint(writer, `{"success":true,"status":200,"data":{"content":[{"Id":"native-tx-1","ServiceId":"service-1","Amount":1.25},{"Id":"native-tx-2","ServiceId":"service-1","Amount":3.25}],"totalElements":2,"totalPages":1,"size":20,"number":0,"last":true}}`)
			case "/api/v1/external/rated-transactions/native-tx-1":
				_, _ = fmt.Fprint(writer, `{"success":true,"status":200,"data":{"id":"native-tx-1","serviceId":"service-1","transactionDate":"2026-08-15","amount":1.25,"currency":"USD","projectId":"project-a","resourceId":"vm-1"}}`)
			case "/api/v1/external/rated-transactions/native-tx-2":
				_, _ = fmt.Fprint(writer, `{"success":true,"status":200,"data":{"id":"native-tx-2","serviceId":"service-1","transactionDate":"2026-08-16","amount":3.25,"currency":"USD","projectId":"project-a","resourceId":"vm-1"}}`)
			default:
				Fail("unexpected M360 path: " + request.URL.Path)
			}
		}))
		DeferCleanup(server.Close)

		client, err := NewClient(Config{BaseURL: server.URL, APIKey: "test-token", APIVersion: "v1", HTTPClient: server.Client()})
		Expect(err).NotTo(HaveOccurred())

		costs, err := client.ListCosts(context.Background(), billing.CostQuery{
			AccountReference: "account-1",
			FromDate:         "2026-08-01",
			ToDate:           "2026-08-31",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(costs).To(Equal([]billing.Cost{{
			ProjectID:    new("project-a"),
			ResourceID:   new("vm-1"),
			Service:      billing.ServiceComputeInstance,
			Amount:       "4.50",
			CurrencyCode: "USD",
			PeriodStart:  "2026-08-01",
			PeriodEnd:    "2026-08-31",
		}}))
	})

	It("preserves missing project attribution instead of treating it as the default project", func() {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			switch request.URL.Path {
			case "/api/v1/external/services":
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"content":[{"Id":"service-1","Code":"compute_instance"}],"last":true}}`)
			case "/api/v1/external/rated-transactions":
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"content":[{"Id":"native-tx-1","ServiceId":"service-1"}],"last":true}}`)
			case "/api/v1/external/rated-transactions/native-tx-1":
				_, _ = fmt.Fprint(writer, `{"success":true,"data":{"id":"native-tx-1","serviceId":"service-1","amount":1.25,"currency":"USD","resourceId":"vm-1"}}`)
			default:
				Fail("unexpected M360 path: " + request.URL.Path)
			}
		}))
		DeferCleanup(server.Close)

		client, err := NewClient(Config{BaseURL: server.URL, APIKey: "test-token", APIVersion: "v1", HTTPClient: server.Client()})
		Expect(err).NotTo(HaveOccurred())

		costs, err := client.ListCosts(context.Background(), billing.CostQuery{
			AccountReference: "account-1",
			FromDate:         "2026-08-01",
			ToDate:           "2026-08-31",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(costs).To(HaveLen(1))
		Expect(costs[0].ProjectID).To(BeNil())
		Expect(costs[0].ResourceID).To(Equal(new("vm-1")))
	})
})
