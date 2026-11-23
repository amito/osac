/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package main

import (
	"crypto/x509"
	"fmt"

	"github.com/osac-project/osac/osac-observability/internal/membership"
)

// membershipCache builds the per-subject project membership cache. An empty
// target disables project scope, in which case a Tenant User sees the default
// project only.
func membershipCache(target, caFile string) (*membership.Cache, func() error, error) {
	if target == "" {
		return nil, nil, nil
	}
	roots, err := fulfillmentRoots(caFile)
	if err != nil {
		return nil, nil, err
	}
	lister, closeConnection, err := membership.NewClient(target, roots)
	if err != nil {
		return nil, nil, fmt.Errorf("build the fulfillment client: %w", err)
	}
	return &membership.Cache{Lister: lister}, closeConnection, nil
}

// fulfillmentRoots loads the CA that verifies the fulfillment endpoint. An
// empty path returns nil so the TLS client keeps the system roots.
func fulfillmentRoots(caFile string) (*x509.CertPool, error) {
	if caFile == "" {
		return nil, nil
	}
	return loadCertPool(caFile)
}
