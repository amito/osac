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
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// selectorName reports the selector's effective metric name and whether it
// constrains __name__ at all. A name written before the braces and an equality
// __name__ matcher both name the metric; any other __name__ matcher constrains
// the name without naming a family.
func selectorName(selector *parser.VectorSelector) (string, bool) {
	if selector.Name != "" {
		return selector.Name, true
	}
	for _, matcher := range selector.LabelMatchers {
		if matcher.Name != metricNameLabel {
			continue
		}
		if matcher.Type == labels.MatchEqual {
			return matcher.Value, true
		}
		return "", true
	}
	return "", false
}

// matrixSelector returns the vector selector inside a matrix. The parser types
// the field as Expr, but a parsed range always wraps exactly one selector.
func matrixSelector(matrix *parser.MatrixSelector) (*parser.VectorSelector, bool) {
	selector, ok := matrix.VectorSelector.(*parser.VectorSelector)
	return selector, ok
}

// Rewrite narrows expr to the scope on the parsed AST. It never rebuilds the
// expression from the caller's query text: every selector is mutated or cloned
// in place, so the caller's metric names, grouping, modifiers and ranges are
// preserved exactly.
//
// A scope branch is one conjunction of label matchers and a scope admits a
// series when any branch matches it. With several branches the nearest
// instant-vector expression is OR-joined; a matrix selector cannot be
// OR-joined, so it is duplicated in the call that encloses it. A form this
// version cannot narrow reports ErrUnrewritable and a caller with no branches
// reports ErrNoScope, so the handler answers with an empty result instead of
// forwarding the original query.
func Rewrite(expr parser.Expr, scope Scope) (parser.Expr, error) {
	if scope.Empty() {
		return nil, ErrNoScope
	}
	return rewriteExpr(expr, scope)
}

func rewriteExpr(expr parser.Expr, scope Scope) (parser.Expr, error) {
	switch node := expr.(type) {
	case *parser.VectorSelector:
		return scopeVectorSelector(node, scope)
	case *parser.MatrixSelector:
		// A matrix is not an instant vector: it cannot carry an OR itself and
		// only the call that encloses it can duplicate it. Reaching one here
		// means there is no such parent.
		return nil, ErrUnrewritable
	case *parser.SubqueryExpr:
		child, err := rewriteExpr(node.Expr, scope)
		if err != nil {
			return nil, err
		}
		node.Expr = child
		return node, nil
	case *parser.Call:
		return rewriteCall(node, scope)
	case *parser.AggregateExpr:
		if node.Param != nil {
			param, err := rewriteExpr(node.Param, scope)
			if err != nil {
				return nil, err
			}
			node.Param = param
		}
		child, err := rewriteExpr(node.Expr, scope)
		if err != nil {
			return nil, err
		}
		node.Expr = child
		return node, nil
	case *parser.BinaryExpr:
		lhs, err := rewriteExpr(node.LHS, scope)
		if err != nil {
			return nil, err
		}
		rhs, err := rewriteExpr(node.RHS, scope)
		if err != nil {
			return nil, err
		}
		node.LHS = lhs
		node.RHS = rhs
		return node, nil
	case *parser.ParenExpr:
		child, err := rewriteExpr(node.Expr, scope)
		if err != nil {
			return nil, err
		}
		node.Expr = child
		return node, nil
	case *parser.UnaryExpr:
		child, err := rewriteExpr(node.Expr, scope)
		if err != nil {
			return nil, err
		}
		node.Expr = child
		return node, nil
	case *parser.StepInvariantExpr:
		child, err := rewriteExpr(node.Expr, scope)
		if err != nil {
			return nil, err
		}
		node.Expr = child
		return node, nil
	default:
		// Number, string and any future leaf carries no selector.
		return expr, nil
	}
}

// scopeVectorSelector applies the scope to one instant-vector leaf. A single
// branch is merged into the existing selector; several branches need one clone
// per branch because a selector admits one conjunction only.
func scopeVectorSelector(selector *parser.VectorSelector, scope Scope) (parser.Expr, error) {
	branches := scope.BranchesFor(selectorName(selector))
	if len(branches) == 0 {
		return nil, ErrNoScope
	}
	if len(branches) == 1 {
		selector.LabelMatchers = append(selector.LabelMatchers, branches[0]...)
		return selector, nil
	}
	expressions := make([]parser.Expr, 0, len(branches))
	for _, branch := range branches {
		clone, err := cloneExpr(selector)
		if err != nil {
			return nil, ErrUnrewritable
		}
		cloned, ok := clone.(*parser.VectorSelector)
		if !ok {
			return nil, ErrUnrewritable
		}
		// Appending keeps every caller matcher: Prometheus ANDs duplicate
		// label matchers, so a conflicting caller value matches nothing
		// rather than replacing the scope.
		cloned.LabelMatchers = append(cloned.LabelMatchers, branch...)
		expressions = append(expressions, cloned)
	}
	return orExpr(expressions), nil
}

// rewriteCall handles a call, which is the nearest instant-vector expression
// around a matrix selector. Range and step are never recomputed: each clone
// carries the range the caller wrote.
func rewriteCall(call *parser.Call, scope Scope) (parser.Expr, error) {
	matrixIndex := -1
	for index, arg := range call.Args {
		if _, ok := arg.(*parser.MatrixSelector); !ok {
			continue
		}
		if matrixIndex >= 0 {
			// Two matrix leaves would need different branch counts in one
			// instant-vector expression; never guess.
			return nil, ErrUnrewritable
		}
		matrixIndex = index
	}
	if matrixIndex < 0 {
		for index, arg := range call.Args {
			rewritten, err := rewriteExpr(arg, scope)
			if err != nil {
				return nil, err
			}
			call.Args[index] = rewritten
		}
		return call, nil
	}
	matrix := call.Args[matrixIndex].(*parser.MatrixSelector)
	selector, ok := matrixSelector(matrix)
	if !ok {
		return nil, ErrUnrewritable
	}
	branches := scope.BranchesFor(selectorName(selector))
	if len(branches) == 0 {
		return nil, ErrNoScope
	}
	if len(branches) == 1 {
		selector.LabelMatchers = append(selector.LabelMatchers, branches[0]...)
		return call, nil
	}
	expressions := make([]parser.Expr, 0, len(branches))
	for _, branch := range branches {
		clone, err := cloneExpr(call)
		if err != nil {
			return nil, ErrUnrewritable
		}
		cloned, ok := clone.(*parser.Call)
		if !ok || len(cloned.Args) != len(call.Args) {
			return nil, ErrUnrewritable
		}
		matrix, ok := cloned.Args[matrixIndex].(*parser.MatrixSelector)
		if !ok {
			return nil, ErrUnrewritable
		}
		selector, ok := matrixSelector(matrix)
		if !ok {
			return nil, ErrUnrewritable
		}
		selector.LabelMatchers = append(selector.LabelMatchers, branch...)
		expressions = append(expressions, cloned)
	}
	return orExpr(expressions), nil
}
