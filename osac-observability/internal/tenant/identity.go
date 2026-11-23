/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

// Package tenant implements the tenant query authorizer: identity extraction,
// scope construction, query rewriting and the five-endpoint HTTP handler.
package tenant

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

var (
	// ErrUnauthenticated reports a request that the tenant proxy did not
	// validate into a usable tenant identity.
	ErrUnauthenticated = errors.New("unauthenticated")

	// ErrMultipleTenants reports a token that names more than one tenant.
	ErrMultipleTenants = errors.New("token names more than one tenant")
)

// Config is the trusted part of the tenant identity contract.
type Config struct {
	// Issuer is the exact issuer the tenant oauth2-proxy accepts. A token from
	// any other issuer is a token the proxy did not validate.
	Issuer string

	// AdminClientID is the interactive admin client. Its tokens are never
	// tenant tokens, even when they carry an organization claim.
	AdminClientID string

	// AdminRole is the realm role that selects organization scope.
	AdminRole string
}

// Caller is the tenant identity extracted from a validated login token.
type Caller struct {
	Tenant  string
	Subject string
	Admin   bool
	Token   string
	Claims  map[string]any
}

// ExtractBearerToken returns the bearer token from the request, if any.
func ExtractBearerToken(request *http.Request) (string, bool) {
	header := request.Header.Get("Authorization")
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

// Identify trusts the claims the tenant oauth2-proxy validated and turns them
// into a caller identity. The signature is deliberately not verified: the
// proxy is the authentication boundary and the NetworkPolicy keeps every other
// pod away from the authorizer.
func Identify(config Config, request *http.Request) (*Caller, error) {
	raw, ok := ExtractBearerToken(request)
	if !ok {
		return nil, ErrUnauthenticated
	}
	claims, err := parseClaims(raw)
	if err != nil {
		return nil, ErrUnauthenticated
	}
	if config.Issuer != "" && stringClaim(claims, "iss") != config.Issuer {
		return nil, ErrUnauthenticated
	}
	if adminClient := config.AdminClientID; adminClient != "" &&
		(stringClaim(claims, "azp") == adminClient || stringClaim(claims, "client_id") == adminClient) {
		return nil, ErrUnauthenticated
	}
	tenants := organizationNames(claims)
	switch len(tenants) {
	case 0:
		return nil, ErrUnauthenticated
	case 1:
	default:
		return nil, ErrMultipleTenants
	}
	subject := stringClaim(claims, "sub")
	if subject == "" {
		subject = stringClaim(claims, "preferred_username")
	}
	if subject == "" {
		return nil, ErrUnauthenticated
	}
	return &Caller{
		Tenant:  tenants[0],
		Subject: subject,
		Admin:   hasRole(claims, config.AdminRole),
		Token:   raw,
		Claims:  claims,
	}, nil
}

func parseClaims(raw string) (map[string]any, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, ErrUnauthenticated
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	claims := map[string]any{}
	if err = json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func stringClaim(claims map[string]any, name string) string {
	value, _ := claims[name].(string)
	return value
}

// organizationNames collects the tenant names from the claims fulfillment
// recognizes: the organization claim (string, array or object), the
// organizations array and the osac_tenant string. Names are deduplicated
// exactly; two spellings that differ only in case are two tenants, so the
// caller is rejected rather than silently narrowed to one of them.
func organizationNames(claims map[string]any) []string {
	var candidates []string
	candidates = append(candidates, namesFromValue(claims["organization"])...)
	candidates = append(candidates, namesFromValue(claims["organizations"])...)
	if tenant := stringClaim(claims, "osac_tenant"); tenant != "" {
		candidates = append(candidates, tenant)
	}
	seen := map[string]bool{}
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		result = append(result, candidate)
	}
	return result
}

func namesFromValue(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case []any:
		var result []string
		for _, item := range typed {
			if name, ok := item.(string); ok {
				result = append(result, name)
			}
		}
		return result
	case map[string]any:
		var result []string
		for name := range typed {
			result = append(result, name)
		}
		return result
	default:
		return nil
	}
}

func hasRole(claims map[string]any, role string) bool {
	if role == "" {
		return false
	}
	if containsString(namesFromValue(claims["roles"]), role) {
		return true
	}
	realmAccess, _ := claims["realm_access"].(map[string]any)
	if realmAccess == nil {
		return false
	}
	return containsString(namesFromValue(realmAccess["roles"]), role)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
