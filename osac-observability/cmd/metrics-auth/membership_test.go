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
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeTemporaryCA(t *testing.T, contents []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("failed to write the CA file: %v", err)
	}
	return path
}

func TestFulfillmentRootsEmptyUsesSystemRoots(t *testing.T) {
	pool, err := fulfillmentRoots("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pool != nil {
		t.Error("an empty path must leave the system roots in place")
	}
}

func TestFulfillmentRootsRejectsMissingFile(t *testing.T) {
	if _, err := fulfillmentRoots(filepath.Join(t.TempDir(), "absent.crt")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestFulfillmentRootsRejectsNonCertificate(t *testing.T) {
	path := writeTemporaryCA(t, []byte("not a certificate"))
	if _, err := fulfillmentRoots(path); err == nil {
		t.Fatal("expected an error")
	}
}

func TestFulfillmentRootsReadsACertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	contents := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: server.Certificate().Raw,
	})
	pool, err := fulfillmentRoots(writeTemporaryCA(t, contents))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pool == nil || len(pool.Subjects()) != 1 {
		t.Errorf("the certificate was not loaded: %v", pool)
	}
}

func TestMembershipCacheDisabledWithoutURL(t *testing.T) {
	cache, closeConnection, err := membershipCache("", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cache != nil || closeConnection != nil {
		t.Error("an empty URL must disable project resolution")
	}
}

func TestMembershipCacheBuildsAClient(t *testing.T) {
	cache, closeConnection, err := membershipCache("127.0.0.1:1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cache == nil || cache.Lister == nil {
		t.Fatal("expected a configured cache")
	}
	if closeConnection == nil {
		t.Fatal("expected a connection closer")
	}
	if err = closeConnection(); err != nil {
		t.Errorf("closing the connection failed: %v", err)
	}
}

func TestMembershipCacheRejectsABadCA(t *testing.T) {
	path := writeTemporaryCA(t, []byte("not a certificate"))
	if _, _, err := membershipCache("127.0.0.1:1", path); err == nil {
		t.Fatal("expected an error")
	}
}
