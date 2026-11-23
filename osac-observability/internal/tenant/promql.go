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
	"github.com/prometheus/prometheus/promql/parser"
)

// promParser is stateless: NewParser returns a value that builds a fresh
// parser per call, so it is safe to share across requests.
var promParser = parser.NewParser(parser.Options{})

// parseExpr parses one PromQL expression.
func parseExpr(input string) (parser.Expr, error) {
	return promParser.ParseExpr(input)
}

// cloneExpr deep-copies an expression by printing and re-parsing it. The
// result shares no tree nodes with the original, so a branch clone can be
// mutated without touching its siblings.
func cloneExpr(expr parser.Expr) (parser.Expr, error) {
	return parseExpr(expr.String())
}

// orExpr joins expressions with the vector "or" operator and wraps the result
// in parentheses. The parentheses matter: "or" binds loosest, so an unwrapped
// disjunction would change the meaning of the expression it replaces.
func orExpr(exprs []parser.Expr) parser.Expr {
	if len(exprs) == 0 {
		return nil
	}
	if len(exprs) == 1 {
		// A single branch needs no disjunction: returning the expression
		// unchanged keeps the caller's query as written.
		return exprs[0]
	}
	result := exprs[0]
	for _, expr := range exprs[1:] {
		result = &parser.BinaryExpr{Op: parser.LOR, LHS: result, RHS: expr}
	}
	return &parser.ParenExpr{Expr: result}
}
