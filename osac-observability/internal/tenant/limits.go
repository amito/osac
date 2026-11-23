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
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Fair-use defaults, per organization. They are deliberately enforced in the
// tenant handler only: the two admin Routes are never throttled or scoped.
const (
	DefaultMaxConcurrentQueries = 10
	DefaultMaxQueryRange        = 24 * time.Hour
	DefaultQueryTimeout         = 2 * time.Minute
)

var (
	// ErrTooManyQueries reports an organization already at its concurrency
	// limit.
	ErrTooManyQueries = errors.New("too many concurrent queries")
	// ErrRangeTooLong reports a query_range span wider than the allowed range.
	ErrRangeTooLong = errors.New("query range exceeds the limit")
)

// Limits bounds one organization's tenant queries. The bounds are fixed after
// construction; only the in-flight counter changes, under mu.
type Limits struct {
	MaxConcurrent int
	MaxRange      time.Duration
	QueryTimeout  time.Duration

	mu       sync.Mutex
	inflight map[string]int
}

// NewLimits validates and builds the three bounds. A non-positive bound would
// silently disable a limit, so it is a configuration error instead.
func NewLimits(maxConcurrent int, maxRange, queryTimeout time.Duration) (*Limits, error) {
	if maxConcurrent <= 0 {
		return nil, fmt.Errorf("maxConcurrentQueries must be positive, got %d", maxConcurrent)
	}
	if maxRange <= 0 {
		return nil, fmt.Errorf("maxQueryRange must be positive, got %s", maxRange)
	}
	if queryTimeout <= 0 {
		return nil, fmt.Errorf("queryTimeout must be positive, got %s", queryTimeout)
	}
	return &Limits{
		MaxConcurrent: maxConcurrent,
		MaxRange:      maxRange,
		QueryTimeout:  queryTimeout,
		inflight:      map[string]int{},
	}, nil
}

// DefaultLimits builds the documented defaults: 10 concurrent queries, a 24h
// range and a 2m timeout per organization.
func DefaultLimits() *Limits {
	limits, err := NewLimits(DefaultMaxConcurrentQueries, DefaultMaxQueryRange, DefaultQueryTimeout)
	if err != nil {
		// The constants above are always valid.
		panic(err)
	}
	return limits
}

// Acquire reserves one concurrency slot for org. The counter is incremented
// before Acquire returns, so two callers can never both see the last slot, and
// release decrements exactly once.
func (l *Limits) Acquire(org string) (release func(), err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight == nil {
		l.inflight = map[string]int{}
	}
	if l.inflight[org] >= l.MaxConcurrent {
		return nil, ErrTooManyQueries
	}
	l.inflight[org]++
	released := false
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if released {
			return
		}
		released = true
		l.inflight[org]--
		if l.inflight[org] <= 0 {
			delete(l.inflight, org)
		}
	}, nil
}

// InFlight reports the caller's current query count. It exists so tests can
// prove a slot is released; nothing in the request path reads it.
func (l *Limits) InFlight(org string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inflight[org]
}

// CheckRange rejects a query_range span wider than MaxRange. start and end are
// Unix seconds, which Prometheus accepts with fractional precision.
func (l *Limits) CheckRange(start, end float64) error {
	if end-start > l.MaxRange.Seconds() {
		return fmt.Errorf("%w: the range may span at most %s", ErrRangeTooLong, l.MaxRange)
	}
	return nil
}

// RangeFromValues parses the start and end of a query_range request. A missing
// or malformed bound is a request error, reported separately from
// ErrRangeTooLong.
func RangeFromValues(values url.Values) (start, end float64, err error) {
	startText, endText := values.Get("start"), values.Get("end")
	if startText == "" || endText == "" {
		return 0, 0, errors.New("the start and end parameters are required")
	}
	start, err = strconv.ParseFloat(startText, 64)
	if err != nil {
		return 0, 0, errors.New("the start parameter is not a timestamp")
	}
	end, err = strconv.ParseFloat(endText, 64)
	if err != nil {
		return 0, 0, errors.New("the end parameter is not a timestamp")
	}
	return start, end, nil
}
