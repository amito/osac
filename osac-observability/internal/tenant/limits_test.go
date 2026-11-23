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
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestLimits(t *testing.T, maxConcurrent int, maxRange, timeout time.Duration) *Limits {
	t.Helper()
	limits, err := NewLimits(maxConcurrent, maxRange, timeout)
	if err != nil {
		t.Fatalf("failed to build the limits: %v", err)
	}
	return limits
}

func TestAcquireRejectsAtLimit(t *testing.T) {
	limits := newTestLimits(t, 2, 24*time.Hour, time.Minute)
	releaseFirst, err := limits.Acquire("acme")
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	releaseSecond, err := limits.Acquire("acme")
	if err != nil {
		t.Fatalf("second acquire failed: %v", err)
	}
	if _, err := limits.Acquire("acme"); !errors.Is(err, ErrTooManyQueries) {
		t.Fatalf("want ErrTooManyQueries, got %v", err)
	}
	releaseFirst()
	if _, err := limits.Acquire("acme"); err != nil {
		t.Errorf("releasing a slot must admit a new query: %v", err)
	}
	releaseSecond()
}

func TestAcquireIsPerOrganization(t *testing.T) {
	limits := newTestLimits(t, 1, 24*time.Hour, time.Minute)
	release, err := limits.Acquire("acme")
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	defer release()
	if _, err := limits.Acquire("other"); err != nil {
		t.Errorf("one organization's limit must not block another: %v", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	limits := newTestLimits(t, 1, 24*time.Hour, time.Minute)
	release, err := limits.Acquire("acme")
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	release()
	release()
	if got := limits.InFlight("acme"); got != 0 {
		t.Errorf("a double release must not go negative: %d", got)
	}
	if _, err := limits.Acquire("acme"); err != nil {
		t.Errorf("the slot must be available again: %v", err)
	}
}

func TestInFlightCountsOnlyTheOrganization(t *testing.T) {
	limits := newTestLimits(t, 3, 24*time.Hour, time.Minute)
	releaseA, err := limits.Acquire("acme")
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	if _, err := limits.Acquire("acme"); err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	if got := limits.InFlight("acme"); got != 2 {
		t.Errorf("want 2 in flight, got %d", got)
	}
	if got := limits.InFlight("other"); got != 0 {
		t.Errorf("want 0 in flight for another organization, got %d", got)
	}
	releaseA()
	if got := limits.InFlight("acme"); got != 1 {
		t.Errorf("want 1 in flight, got %d", got)
	}
}

func TestRangeWithinLimit(t *testing.T) {
	limits := newTestLimits(t, 1, 24*time.Hour, time.Minute)
	if err := limits.CheckRange(0, 3600); err != nil {
		t.Errorf("one hour must be admitted: %v", err)
	}
	if err := limits.CheckRange(0, 24*3600); err != nil {
		t.Errorf("a span exactly at the limit must be admitted: %v", err)
	}
	if err := limits.CheckRange(100.5, 100.5+3600); err != nil {
		t.Errorf("fractional seconds must be admitted: %v", err)
	}
}

func TestRangeOverLimit(t *testing.T) {
	limits := newTestLimits(t, 1, 24*time.Hour, time.Minute)
	if err := limits.CheckRange(0, 25*3600); !errors.Is(err, ErrRangeTooLong) {
		t.Errorf("want ErrRangeTooLong, got %v", err)
	}
}

func TestRangeFromQueryRange(t *testing.T) {
	start, end, err := RangeFromValues(url.Values{"start": {"0"}, "end": {"90000"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if start != 0 || end != 90000 {
		t.Errorf("unexpected bounds: %v %v", start, end)
	}
	if _, _, err := RangeFromValues(url.Values{"start": {"0.5"}, "end": {"3600.25"}}); err != nil {
		t.Errorf("fractional seconds must be accepted: %v", err)
	}
}

func TestRangeFromQueryRangeRejectsMissingOrMalformed(t *testing.T) {
	for name, values := range map[string]url.Values{
		"missing end":     {"start": {"0"}},
		"missing start":   {"end": {"1"}},
		"malformed end":   {"start": {"0"}, "end": {"later"}},
		"malformed start": {"start": {"now"}, "end": {"1"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := RangeFromValues(values); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestLimitConfigValidation(t *testing.T) {
	tests := []struct {
		name          string
		maxConcurrent int
		maxRange      time.Duration
		timeout       time.Duration
		want          string
	}{
		{name: "concurrency zero", maxConcurrent: 0, maxRange: time.Hour, timeout: time.Minute, want: "maxConcurrentQueries"},
		{name: "concurrency negative", maxConcurrent: -1, maxRange: time.Hour, timeout: time.Minute, want: "maxConcurrentQueries"},
		{name: "range zero", maxConcurrent: 1, maxRange: 0, timeout: time.Minute, want: "maxQueryRange"},
		{name: "range negative", maxConcurrent: 1, maxRange: -time.Hour, timeout: time.Minute, want: "maxQueryRange"},
		{name: "timeout zero", maxConcurrent: 1, maxRange: time.Hour, timeout: 0, want: "queryTimeout"},
		{name: "timeout negative", maxConcurrent: 1, maxRange: time.Hour, timeout: -time.Minute, want: "queryTimeout"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewLimits(test.maxConcurrent, test.maxRange, test.timeout)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("expected the error to name %s, got %v", test.want, err)
			}
		})
	}
}

func TestDefaultLimits(t *testing.T) {
	limits := DefaultLimits()
	if limits.MaxConcurrent != 10 {
		t.Errorf("want 10 concurrent queries, got %d", limits.MaxConcurrent)
	}
	if limits.MaxRange != 24*time.Hour {
		t.Errorf("want a 24h range, got %s", limits.MaxRange)
	}
	if limits.QueryTimeout != 2*time.Minute {
		t.Errorf("want a 2m timeout, got %s", limits.QueryTimeout)
	}
}
