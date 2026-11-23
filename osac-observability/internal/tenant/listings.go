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
	"fmt"
	"strings"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// ExpandMatch returns one selector per caller selector and scope branch, in
// caller-then-branch order, so a listing forwards every admitted conjunction
// and nothing else. Absent or empty match[] means "every series" to
// Prometheus, which is exactly the request that must never be forwarded: it is
// replaced by the scope selectors. A caller selector that is not a plain
// series selector is an error rather than a pass-through.
func ExpandMatch(values []string, scope Scope) ([]string, error) {
	if scope.Empty() {
		return nil, ErrNoScope
	}
	if len(values) == 0 {
		values = []string{""}
	}
	var result []string
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			for _, branch := range scope.BranchesFor("", false) {
				result = append(result, (&parser.VectorSelector{
					LabelMatchers: append([]*labels.Matcher(nil), branch...),
				}).String())
			}
			continue
		}
		expr, err := parseExpr(value)
		if err != nil {
			return nil, fmt.Errorf("invalid match[] selector: %w", err)
		}
		selector, ok := expr.(*parser.VectorSelector)
		if !ok {
			return nil, fmt.Errorf("match[] must be a series selector")
		}
		for _, branch := range scope.BranchesFor(selectorName(selector)) {
			clone, err := cloneExpr(selector)
			if err != nil {
				return nil, ErrUnrewritable
			}
			clonedSelector, ok := clone.(*parser.VectorSelector)
			if !ok {
				return nil, ErrUnrewritable
			}
			clonedSelector.LabelMatchers = append(clonedSelector.LabelMatchers, branch...)
			result = append(result, clonedSelector.String())
		}
	}
	if len(result) == 0 {
		// Every selector was unmatchable under this scope. An empty match[]
		// list would mean "every series" to the backend, so never forward it.
		return nil, ErrNoScope
	}
	return result, nil
}
