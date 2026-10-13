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

package billingassignment

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/osac-project/osac/fulfillment-service/internal/billing"
	privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"
)

type providerStub struct {
	accounts map[string]billing.Account
	updates  []string
	getErr   error
}

func (*providerStub) GetStatus(context.Context) (billing.ProviderStatus, error) {
	return billing.ProviderStatus{State: billing.ProviderStateAvailable}, nil
}

func (*providerStub) ListAccounts(context.Context) ([]billing.Account, error) {
	return nil, nil
}

func (p *providerStub) GetAccount(_ context.Context, reference string) (billing.Account, error) {
	if p.getErr != nil {
		return billing.Account{}, p.getErr
	}
	account, ok := p.accounts[reference]
	if !ok {
		return billing.Account{}, errors.New("not found")
	}
	return account, nil
}

func (p *providerStub) UpdateAccountExternalID(_ context.Context, reference, expectedExternalID, externalID string) error {
	account := p.accounts[reference]
	if account.ExternalID != expectedExternalID {
		return billing.ErrAccountExternalIDChanged
	}
	p.updates = append(p.updates, reference+":"+externalID)
	account.ExternalID = externalID
	p.accounts[reference] = account
	return nil
}

type assignmentClientStub struct {
	items        []*privatev1.TenantBillingAssignment
	state        privatev1.TenantBillingAssignmentConnectionState
	message      string
	statusUpdate int
}

func (c *assignmentClientStub) List(_ context.Context, _ *privatev1.TenantBillingAssignmentsListRequest,
	_ ...grpc.CallOption) (*privatev1.TenantBillingAssignmentsListResponse, error) {
	return privatev1.TenantBillingAssignmentsListResponse_builder{Items: c.items, Total: int32(len(c.items))}.Build(), nil
}

func (c *assignmentClientStub) UpdateStatus(_ context.Context, request *privatev1.TenantBillingAssignmentsUpdateStatusRequest,
	_ ...grpc.CallOption) (*privatev1.TenantBillingAssignmentsUpdateStatusResponse, error) {
	c.state = request.GetConnectionState()
	c.message = request.GetMessage()
	c.statusUpdate++
	return privatev1.TenantBillingAssignmentsUpdateStatusResponse_builder{}.Build(), nil
}

type accountClientStub map[string]string

func (c accountClientStub) Get(_ context.Context, request *privatev1.BillingAccountsGetRequest,
	_ ...grpc.CallOption) (*privatev1.BillingAccountsGetResponse, error) {
	reference, ok := c[request.GetId()]
	if !ok {
		return nil, errors.New("not found")
	}
	return privatev1.BillingAccountsGetResponse_builder{
		Object: privatev1.BillingAccount_builder{ProviderAccountReference: reference}.Build(),
	}.Build(), nil
}

func assignment(id, tenant, accountID string, closed bool) *privatev1.TenantBillingAssignment {
	status := privatev1.TenantBillingAssignmentStatus_builder{
		ConnectionState: privatev1.TenantBillingAssignmentConnectionState_TENANT_BILLING_ASSIGNMENT_CONNECTION_STATE_PENDING,
	}.Build()
	if closed {
		status.SetEffectiveTo(timestamppbNow())
	}
	return privatev1.TenantBillingAssignment_builder{
		Id:     id,
		Spec:   privatev1.TenantBillingAssignmentSpec_builder{TenantId: tenant, BillingAccountId: accountID}.Build(),
		Status: status,
	}.Build()
}

func timestamppbNow() *timestamppb.Timestamp {
	return timestamppb.New(time.Now())
}

func newTestFunction(provider *providerStub, assignments *assignmentClientStub, accounts accountClientStub) *Function {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	function, err := NewFunction().SetLogger(logger).SetProvider(provider).SetClients(assignments, accounts).Build()
	if err != nil {
		panic(err)
	}
	return function
}

func TestReconcileLinksAndVerifiesAssignment(t *testing.T) {
	provider := &providerStub{accounts: map[string]billing.Account{
		"native-new": {Reference: "native-new", State: billing.AccountStateActive},
	}}
	assignments := &assignmentClientStub{}
	function := newTestFunction(provider, assignments, accountClientStub{"osac-new": "native-new"})
	current := assignment("assignment-new", "tenant-a", "osac-new", false)
	assignments.items = []*privatev1.TenantBillingAssignment{current}

	if err := function.Reconcile(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if got, want := provider.accounts["native-new"].ExternalID, "tenant-a"; got != want {
		t.Fatalf("external ID = %q, want %q", got, want)
	}
	if assignments.state != privatev1.TenantBillingAssignmentConnectionState_TENANT_BILLING_ASSIGNMENT_CONNECTION_STATE_CONNECTED {
		t.Fatalf("connection state = %v, want CONNECTED", assignments.state)
	}
	if !reflect.DeepEqual(provider.updates, []string{"native-new:tenant-a"}) {
		t.Fatalf("provider updates = %v", provider.updates)
	}
}

func TestReconcileClearsPreviousAccountBeforeLinkingReplacement(t *testing.T) {
	provider := &providerStub{accounts: map[string]billing.Account{
		"native-old": {Reference: "native-old", ExternalID: "tenant-a", State: billing.AccountStateActive},
		"native-new": {Reference: "native-new", State: billing.AccountStateActive},
	}}
	assignments := &assignmentClientStub{}
	function := newTestFunction(provider, assignments, accountClientStub{
		"osac-old": "native-old",
		"osac-new": "native-new",
	})
	old := assignment("assignment-old", "tenant-a", "osac-old", true)
	current := assignment("assignment-new", "tenant-a", "osac-new", false)
	assignments.items = []*privatev1.TenantBillingAssignment{old, current}

	if err := function.Reconcile(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	want := []string{"native-old:", "native-new:tenant-a"}
	if !reflect.DeepEqual(provider.updates, want) {
		t.Fatalf("provider update order = %v, want %v", provider.updates, want)
	}
}

func TestReconcileNeverOverwritesAccountBoundToAnotherTenant(t *testing.T) {
	provider := &providerStub{accounts: map[string]billing.Account{
		"native": {Reference: "native", ExternalID: "tenant-b", State: billing.AccountStateActive},
	}}
	assignments := &assignmentClientStub{}
	function := newTestFunction(provider, assignments, accountClientStub{"osac": "native"})
	current := assignment("assignment", "tenant-a", "osac", false)
	assignments.items = []*privatev1.TenantBillingAssignment{current}

	err := function.Reconcile(context.Background(), current)
	if err == nil {
		t.Fatal("Reconcile() succeeded for an account bound to another tenant")
	}
	if len(provider.updates) != 0 {
		t.Fatalf("provider updates = %v, want no update", provider.updates)
	}
	if assignments.message != "ACCOUNT_ALREADY_BOUND" {
		t.Fatalf("pending message = %q, want ACCOUNT_ALREADY_BOUND", assignments.message)
	}
}
