/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

// Package membership resolves which projects a tenant user belongs to through
// fulfillment's public ProjectMemberships API, using the caller's own token.
package membership

import (
	"context"
	"sort"
	"sync"
	"time"
)

// DefaultTTL bounds how long a resolved project set is reused.
const DefaultTTL = 30 * time.Second

// Lister resolves the project set of one subject. The token is the caller's
// bearer token, forwarded unchanged: the authorizer is not an identity
// authority and never mints credentials for this call.
type Lister func(ctx context.Context, subject, token string) ([]string, error)

type entry struct {
	projects  []string
	fetchedAt time.Time
}

// refresh coalesces concurrent resolutions of one subject: the first caller
// fetches, and the others wait for its result instead of calling fulfillment
// once per request.
type refresh struct {
	done     chan struct{}
	projects []string
	err      error
}

// Cache serves a subject's project set for at most TTL. An entry is discarded
// at expiry whether or not the refresh succeeds: a failed refresh is reported
// and never answered with the stale set.
type Cache struct {
	TTL    time.Duration
	Now    func() time.Time
	Lister Lister

	mu         sync.Mutex
	entries    map[string]*entry
	refreshing map[string]*refresh
}

// Projects returns the subject's project set. On a cold failure, or a failed
// refresh, it returns nil and the error; the handler substitutes the caller's
// default project rather than failing the request.
func (c *Cache) Projects(ctx context.Context, subject, token string) ([]string, error) {
	now := c.now()
	c.mu.Lock()
	if cached := c.entries[subject]; cached != nil && now.Sub(cached.fetchedAt) < c.ttl() {
		projects := append([]string(nil), cached.projects...)
		c.mu.Unlock()
		return projects, nil
	}
	if pending := c.refreshing[subject]; pending != nil {
		c.mu.Unlock()
		select {
		case <-pending.done:
			return append([]string(nil), pending.projects...), pending.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	pending := &refresh{done: make(chan struct{})}
	if c.refreshing == nil {
		c.refreshing = map[string]*refresh{}
	}
	c.refreshing[subject] = pending
	// The expired entry is dropped before the fetch. A concurrent caller past
	// the window joins this refresh rather than reading the stale set.
	delete(c.entries, subject)
	c.mu.Unlock()

	projects, err := c.Lister(ctx, subject, token)
	if err != nil {
		projects = nil
	} else {
		projects = sortedUnique(projects)
	}
	c.mu.Lock()
	if err == nil {
		if c.entries == nil {
			c.entries = map[string]*entry{}
		}
		c.entries[subject] = &entry{projects: projects, fetchedAt: c.now()}
	}
	pending.projects, pending.err = projects, err
	delete(c.refreshing, subject)
	c.mu.Unlock()
	close(pending.done)
	return append([]string(nil), projects...), err
}

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Cache) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return DefaultTTL
}

func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
