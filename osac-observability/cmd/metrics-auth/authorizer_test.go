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
	"strings"
	"testing"
	"time"
)

func emptyEnv(string) string { return "" }

func validAuthorizerArgs() []string {
	return []string{
		"--upstream=http://thanos-query.osac-observability.svc.cluster.local:9090",
		"--issuer=https://keycloak.example.com/realms/osac",
		"--admin-client-id=thanos-admin",
		"--admin-role=tenant-admin",
	}
}

func TestLoadAuthorizerConfigDefaults(t *testing.T) {
	cfg, err := loadAuthorizerConfig(validAuthorizerArgs(), emptyEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("expected the default listen address, got %q", cfg.Listen)
	}
	if cfg.InventoryRefresh != 30*time.Second {
		t.Errorf("expected a 30s refresh interval, got %s", cfg.InventoryRefresh)
	}
	if cfg.AllowlistPath != "" {
		t.Errorf("expected the built-in allowlist, got %q", cfg.AllowlistPath)
	}
	if cfg.Upstream.Host != "thanos-query.osac-observability.svc.cluster.local:9090" {
		t.Errorf("unexpected upstream %q", cfg.Upstream)
	}
	if cfg.Limits == nil {
		t.Fatal("expected default limits")
	}
	if cfg.Limits.MaxConcurrent != 10 || cfg.Limits.MaxRange != 24*time.Hour ||
		cfg.Limits.QueryTimeout != 2*time.Minute {
		t.Errorf("unexpected default limits: %+v", cfg.Limits)
	}
}

func TestLoadAuthorizerConfigAcceptsLimitOverrides(t *testing.T) {
	cfg, err := loadAuthorizerConfig(append(validAuthorizerArgs(),
		"--max-concurrent=3", "--max-range=6h", "--query-timeout=30s"), emptyEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Limits.MaxConcurrent != 3 || cfg.Limits.MaxRange != 6*time.Hour ||
		cfg.Limits.QueryTimeout != 30*time.Second {
		t.Errorf("the overrides were not applied: %+v", cfg.Limits)
	}
}

func TestLoadAuthorizerConfigRejectsBadLimits(t *testing.T) {
	for name, args := range map[string][]string{
		"concurrency zero":   {"--max-concurrent=0"},
		"concurrency neg":    {"--max-concurrent=-1"},
		"range zero":         {"--max-range=0s"},
		"timeout zero":       {"--query-timeout=0s"},
		"range not duration": {"--max-range=day"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadAuthorizerConfig(append(validAuthorizerArgs(), args...), emptyEnv); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestLoadAuthorizerConfigRequiresFlags(t *testing.T) {
	tests := []struct {
		name   string
		remove string
		want   string
	}{
		{name: "upstream", remove: "--upstream=http://thanos-query.osac-observability.svc.cluster.local:9090", want: "--upstream"},
		{name: "issuer", remove: "--issuer=https://keycloak.example.com/realms/osac", want: "--issuer"},
		{name: "admin client", remove: "--admin-client-id=thanos-admin", want: "--admin-client-id"},
		{name: "admin role", remove: "--admin-role=tenant-admin", want: "--admin-role"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var args []string
			for _, arg := range validAuthorizerArgs() {
				if arg != test.remove {
					args = append(args, arg)
				}
			}
			_, err := loadAuthorizerConfig(args, emptyEnv)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("expected the error to name %s, got %v", test.want, err)
			}
		})
	}
}

func TestLoadAuthorizerConfigRejectsBadValues(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "relative upstream", args: []string{"--upstream=thanos:9090"}},
		{name: "ftp upstream", args: []string{"--upstream=ftp://thanos:9090"}},
		{name: "refresh too long", args: []string{"--inventory-refresh=31s"}},
		{name: "refresh zero", args: []string{"--inventory-refresh=0s"}},
		{name: "unknown flag", args: []string{"--probe-listen=:8081"}},
		{name: "unexpected argument", args: []string{"extra"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := append(validAuthorizerArgs(), test.args...)
			if _, err := loadAuthorizerConfig(args, emptyEnv); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestLoadAuthorizerConfigHasNoSecondListener(t *testing.T) {
	// The authorizer has no probe listener: health endpoints share the main
	// listener, so there is no second port to reach around the proxy.
	if _, err := loadAuthorizerConfig(append(validAuthorizerArgs(), "--probe-listen=:8081"), emptyEnv); err == nil {
		t.Fatal("a probe listener must not be accepted")
	}
}

func TestLoadAuthorizerConfigAcceptsTheAllowlistPath(t *testing.T) {
	args := append(validAuthorizerArgs(), "--allowlist=/etc/query-allowlist/endpoints.txt")
	cfg, err := loadAuthorizerConfig(args, emptyEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AllowlistPath != "/etc/query-allowlist/endpoints.txt" {
		t.Errorf("unexpected allowlist path %q", cfg.AllowlistPath)
	}
}
