/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package tenant

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

const testIssuer = "https://keycloak.example.com/realms/osac"

func token(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatalf("failed to marshal header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("failed to marshal claims: %v", err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	return encode(header) + "." + encode(payload) + ".signature"
}

func requestWithToken(token string) *http.Request {
	request, _ := http.NewRequest(http.MethodGet, "http://authorizer/api/v1/query", nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request
}

func identify(t *testing.T, claims map[string]any) (*Caller, error) {
	t.Helper()
	return Identify(Config{Issuer: testIssuer, AdminClientID: "thanos-admin", AdminRole: "tenant-admin"},
		requestWithToken(token(t, claims)))
}

func TestIdentifyTenant(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{
			"iss":                testIssuer,
			"sub":                "user-1",
			"preferred_username": "alice",
			"organization":       "acme",
		}
	}
	tests := []struct {
		name    string
		mutate  func(map[string]any)
		token   string
		want    string
		wantErr error
	}{
		{name: "no token", token: " ", wantErr: ErrUnauthenticated},
		{name: "not a jwt", token: "abc", wantErr: ErrUnauthenticated},
		{
			name:    "no organization claim",
			mutate:  func(c map[string]any) { delete(c, "organization") },
			wantErr: ErrUnauthenticated,
		},
		{name: "organization string", want: "acme"},
		{
			name:   "organization one element array",
			mutate: func(c map[string]any) { c["organization"] = []any{"acme"} },
			want:   "acme",
		},
		{
			name:   "organization object keys",
			mutate: func(c map[string]any) { c["organization"] = map[string]any{"acme": true} },
			want:   "acme",
		},
		{
			name:    "organization two entries",
			mutate:  func(c map[string]any) { c["organization"] = []any{"acme", "globex"} },
			wantErr: ErrMultipleTenants,
		},
		{
			name:    "organization two entries differing in case",
			mutate:  func(c map[string]any) { c["organization"] = []any{"Acme", "acme"} },
			wantErr: ErrMultipleTenants,
		},
		{
			name: "organizations array fallback",
			mutate: func(c map[string]any) {
				delete(c, "organization")
				c["organizations"] = []any{"acme"}
			},
			want: "acme",
		},
		{
			name: "osac_tenant fallback",
			mutate: func(c map[string]any) {
				delete(c, "organization")
				c["osac_tenant"] = "acme"
			},
			want: "acme",
		},
		{
			name:    "unregistered issuer",
			mutate:  func(c map[string]any) { c["iss"] = "https://other.example.com/realms/osac" },
			wantErr: ErrUnauthenticated,
		},
		{
			name:    "missing issuer",
			mutate:  func(c map[string]any) { delete(c, "iss") },
			wantErr: ErrUnauthenticated,
		},
		{
			name:    "admin client azp",
			mutate:  func(c map[string]any) { c["azp"] = "thanos-admin" },
			wantErr: ErrUnauthenticated,
		},
		{
			name:    "admin client client_id",
			mutate:  func(c map[string]any) { c["client_id"] = "thanos-admin" },
			wantErr: ErrUnauthenticated,
		},
		{
			name: "tenant admin role",
			mutate: func(c map[string]any) {
				c["realm_access"] = map[string]any{"roles": []any{"tenant-admin"}}
			},
		},
		{
			name:   "tenant admin top level roles",
			mutate: func(c map[string]any) { c["roles"] = []any{"tenant-admin", "other"} },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := base()
			if test.mutate != nil {
				test.mutate(claims)
			}
			var got *Caller
			var err error
			if test.token != "" {
				got, err = Identify(Config{Issuer: testIssuer, AdminClientID: "thanos-admin", AdminRole: "tenant-admin"},
					requestWithToken(test.token))
			} else {
				got, err = identify(t, claims)
			}
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("expected error %v, got %v", test.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if test.want != "" && got.Tenant != test.want {
				t.Errorf("expected tenant %q, got %q", test.want, got.Tenant)
			}
			if got.Subject != "user-1" {
				t.Errorf("expected subject user-1, got %q", got.Subject)
			}
			wantAdmin := test.name == "tenant admin role" || test.name == "tenant admin top level roles"
			if got.Admin != wantAdmin {
				t.Errorf("expected admin %v, got %v", wantAdmin, got.Admin)
			}
			if got.Token == "" {
				t.Error("expected the bearer token to be retained")
			}
		})
	}
}

func TestIdentifySubjectFallsBackToUsername(t *testing.T) {
	caller, err := identify(t, map[string]any{
		"iss":                testIssuer,
		"organization":       "acme",
		"preferred_username": "alice",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if caller.Subject != "alice" {
		t.Errorf("expected subject alice, got %q", caller.Subject)
	}
}

func TestIdentifyRequiresASubject(t *testing.T) {
	_, err := identify(t, map[string]any{"iss": testIssuer, "organization": "acme"})
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

func TestExtractBearerToken(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
		wantOK bool
	}{
		{name: "bearer", header: "Bearer abc", want: "abc", wantOK: true},
		{name: "lowercase scheme", header: "bearer abc", want: "abc", wantOK: true},
		{name: "missing", header: "", wantOK: false},
		{name: "basic", header: "Basic abc", wantOK: false},
		{name: "empty bearer", header: "Bearer ", wantOK: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, _ := http.NewRequest(http.MethodGet, "http://authorizer/", nil)
			if test.header != "" {
				request.Header.Set("Authorization", test.header)
			}
			got, ok := ExtractBearerToken(request)
			if ok != test.wantOK || got != test.want {
				t.Errorf("expected (%q, %v), got (%q, %v)", test.want, test.wantOK, got, ok)
			}
		})
	}
}
