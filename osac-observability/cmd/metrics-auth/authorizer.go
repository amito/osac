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
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/osac-project/osac/osac-observability/internal/allowlist"
	"github.com/osac-project/osac/osac-observability/internal/inventory"
	"github.com/osac-project/osac/osac-observability/internal/tenant"
	"k8s.io/client-go/rest"
)

const defaultAuthorizerListen = ":8080"

type authorizerConfig struct {
	Listen            string
	Upstream          *url.URL
	Issuer            string
	AdminClientID     string
	AdminRole         string
	AllowlistPath     string
	InventoryRefresh  time.Duration
	Limits            *tenant.Limits
	FulfillmentURL    string
	FulfillmentCAFile string
}

func loadAuthorizerConfig(args []string, getenv func(string) string) (authorizerConfig, error) {
	flags := flag.NewFlagSet("authorizer", flag.ContinueOnError)
	listen := flags.String("listen", envOr(getenv, "OSAC_AUTHORIZER_LISTEN", defaultAuthorizerListen), "address to serve the tenant front door on")
	upstreamText := flags.String("upstream", envOr(getenv, "OSAC_AUTHORIZER_UPSTREAM", ""), "Thanos Query base URL (required)")
	issuer := flags.String("issuer", envOr(getenv, "OSAC_AUTHORIZER_ISSUER", ""), "issuer the tenant proxy is trusted to accept (required)")
	adminClient := flags.String("admin-client-id", envOr(getenv, "OSAC_AUTHORIZER_ADMIN_CLIENT_ID", ""), "admin client whose tokens are not tenant tokens (required)")
	adminRole := flags.String("admin-role", envOr(getenv, "OSAC_AUTHORIZER_ADMIN_ROLE", ""), "realm role that selects organization scope (required)")
	allowlistPath := flags.String("allowlist", envOr(getenv, "OSAC_AUTHORIZER_ALLOWLIST", ""), "allowlist file; empty uses the built-in five endpoints")
	refresh := flags.Duration("inventory-refresh", inventory.DefaultRefreshInterval, "inventory re-list interval; at most 30s")
	maxConcurrent := flags.Int("max-concurrent", tenant.DefaultMaxConcurrentQueries, "maximum concurrent queries per organization")
	maxRange := flags.Duration("max-range", tenant.DefaultMaxQueryRange, "maximum query_range span per organization")
	queryTimeout := flags.Duration("query-timeout", tenant.DefaultQueryTimeout, "timeout of one forwarded tenant query")
	fulfillmentURL := flags.String("fulfillment-url", envOr(getenv, "OSAC_AUTHORIZER_FULFILLMENT_URL", ""), "fulfillment public API URL for project memberships; empty disables project scope")
	fulfillmentCA := flags.String("fulfillment-ca-file", envOr(getenv, "OSAC_AUTHORIZER_FULFILLMENT_CA_FILE", ""), "CA bundle that verifies the fulfillment endpoint")
	if err := flags.Parse(args); err != nil {
		return authorizerConfig{}, err
	}
	if flags.NArg() > 0 {
		return authorizerConfig{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *upstreamText == "" {
		return authorizerConfig{}, errors.New("the --upstream flag is required")
	}
	upstream, err := url.Parse(*upstreamText)
	if err != nil || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Host == "" {
		return authorizerConfig{}, fmt.Errorf("the --upstream value %q must be an absolute http(s) URL", *upstreamText)
	}
	if *issuer == "" {
		return authorizerConfig{}, errors.New("the --issuer flag is required")
	}
	if *adminClient == "" {
		return authorizerConfig{}, errors.New("the --admin-client-id flag is required")
	}
	if *adminRole == "" {
		return authorizerConfig{}, errors.New("the --admin-role flag is required")
	}
	if *refresh <= 0 || *refresh > inventory.DefaultRefreshInterval {
		return authorizerConfig{}, fmt.Errorf("the --inventory-refresh value %s must be positive and at most %s",
			*refresh, inventory.DefaultRefreshInterval)
	}
	if *fulfillmentURL != "" {
		parsed, err := url.Parse(*fulfillmentURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return authorizerConfig{}, fmt.Errorf("the --fulfillment-url value %q must be an absolute https URL",
				*fulfillmentURL)
		}
	}
	limits, err := tenant.NewLimits(*maxConcurrent, *maxRange, *queryTimeout)
	if err != nil {
		return authorizerConfig{}, fmt.Errorf("invalid tenant limits: %w", err)
	}
	return authorizerConfig{
		Listen:            *listen,
		Upstream:          upstream,
		Issuer:            *issuer,
		AdminClientID:     *adminClient,
		AdminRole:         *adminRole,
		AllowlistPath:     *allowlistPath,
		InventoryRefresh:  *refresh,
		Limits:            limits,
		FulfillmentURL:    *fulfillmentURL,
		FulfillmentCAFile: *fulfillmentCA,
	}, nil
}

func runAuthorizer(cfg authorizerConfig) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("build the in-cluster Kubernetes client: %w", err)
	}
	source, err := inventory.NewKubernetesSource(restConfig)
	if err != nil {
		return fmt.Errorf("build the inventory source: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store := inventory.NewStore(inventory.StoreConfig{
		Source:          source,
		RefreshInterval: cfg.InventoryRefresh,
	})
	store.Run(ctx)

	matcher := allowlist.Default()
	if cfg.AllowlistPath != "" {
		matcher, err = allowlist.Load(cfg.AllowlistPath)
		if err != nil {
			return err
		}
	}

	memberships, closeMemberships, membershipErr := membershipCache(
		cfg.FulfillmentURL, cfg.FulfillmentCAFile)
	if membershipErr != nil {
		// A resolution outage narrows Tenant Users to their default project; it
		// must not take the Route down.
		logger.Warn("project membership resolution is disabled", "error", membershipErr)
	}
	if closeMemberships != nil {
		defer func() {
			if err := closeMemberships(); err != nil {
				logger.Warn("closing the fulfillment client failed", "error", err)
			}
		}()
	}

	handler, err := tenant.NewHandler(tenant.HandlerConfig{
		Identity: tenant.Config{
			Issuer:        cfg.Issuer,
			AdminClientID: cfg.AdminClientID,
			AdminRole:     cfg.AdminRole,
		},
		Inventory:   store,
		Upstream:    cfg.Upstream,
		Allow:       matcher,
		Logger:      logger,
		Limits:      cfg.Limits,
		Memberships: memberships,
	})
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	result := make(chan error, 1)
	go func() {
		logger.Info("serving the tenant authorizer", "listen", cfg.Listen, "upstream", cfg.Upstream.String())
		result <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		logger.Info("shutting down the tenant authorizer")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultShutdownGraceTime)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err = <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
