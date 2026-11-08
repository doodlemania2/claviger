package search

// Fork-local addition — not upstream PocketBase (issue #106).
//
// The filter functions whose SQL has to differ per database engine. SQLite
// uses the upstream implementations in token_functions.go (the reference);
// a dialect.FunctionBuilder provides the others.

import (
	"errors"
	"fmt"
	"slices"

	"ganigeorgiev.com/fexpr"
	"ganigeorgiev.com/pocketbase/dbx"
	"github.com/doodlemania2/claviger/tools/dialect"
)

type builderTokenFunc = func(
	d dialect.Dialect,
	fb dialect.FunctionBuilder,
	argTokenResolverFunc func(fexpr.Token) (*ResolverResult, error),
	args ...fexpr.Token,
) (*ResolverResult, error)

var builderTokenFunctions = map[string]builderTokenFunc{
	// Same contract as the SQLite geoDistance in token_functions.go.
	"geoDistance": func(d dialect.Dialect, fb dialect.FunctionBuilder, argTokenResolverFunc func(fexpr.Token) (*ResolverResult, error), args ...fexpr.Token) (*ResolverResult, error) {
		if len(args) != 4 {
			return nil, fmt.Errorf("[geoDistance] expected 4 arguments, got %d", len(args))
		}

		resolvedArgs := make([]*ResolverResult, 4)
		coords := make([]string, 4)
		for i, arg := range args {
			if arg.Type != fexpr.TokenIdentifier && arg.Type != fexpr.TokenNumber {
				return nil, fmt.Errorf("[geoDistance] argument %d must be an identifier or number", i)
			}
			resolved, err := argTokenResolverFunc(arg)
			if err != nil {
				return nil, fmt.Errorf("[geoDistance] failed to resolve argument %d: %w", i, err)
			}
			resolvedArgs[i] = resolved
			coords[i] = numericOperand(d, resolved)
		}

		return &ResolverResult{
			NullFallback: NullFallbackDisabled,
			Identifier:   fb.GeoDistance(coords[0], coords[1], coords[2], coords[3]),
			Params:       mergeParams(resolvedArgs[0].Params, resolvedArgs[1].Params, resolvedArgs[2].Params, resolvedArgs[3].Params),
		}, nil
	},

	// Same contract as the SQLite strftime in token_functions.go, with the
	// format and modifiers translated at build time (they are always string
	// literals), see dialect.FunctionBuilder.StrFTime for what is supported.
	"strftime": func(d dialect.Dialect, fb dialect.FunctionBuilder, argTokenResolverFunc func(fexpr.Token) (*ResolverResult, error), args ...fexpr.Token) (*ResolverResult, error) {
		totalArgs := len(args)

		if totalArgs < 1 {
			return nil, fmt.Errorf("[strftime] expected at least 1 arguments, got %d", len(args))
		}

		// limit the number of arguments to prevent abuse
		if totalArgs > 10 {
			return nil, fmt.Errorf("[strftime] too many arguments (max allowed 10, got %d)", totalArgs)
		}

		if args[0].Type != fexpr.TokenText {
			return nil, errors.New("[strftime] expects the first argument to be a format string")
		}

		var modifiers []string
		for i, arg := range args[min(2, totalArgs):] {
			if arg.Type != fexpr.TokenText {
				return nil, fmt.Errorf("[strftime] invalid modifier argument %d - can be only string", i)
			}
			modifiers = append(modifiers, arg.Literal)
		}

		// no time-value: now
		if totalArgs == 1 {
			sql, err := fb.StrFTime(args[0].Literal, "", false, nil)
			if err != nil {
				return nil, fmt.Errorf("[strftime] %w", err)
			}

			return &ResolverResult{NullFallback: NullFallbackEnforced, Identifier: sql}, nil
		}

		allowedTimeValueTokens := []fexpr.TokenType{fexpr.TokenText, fexpr.TokenIdentifier, fexpr.TokenNumber}
		if !slices.Contains(allowedTimeValueTokens, args[1].Type) {
			return nil, errors.New("[strftime] expects the second argument to be of a valid time-value type")
		}

		timeValueArgResult, err := argTokenResolverFunc(args[1])
		if err != nil {
			return nil, fmt.Errorf("[strftime] failed to resolve time-value argument: %w", err)
		}

		isNumber := args[1].Type == fexpr.TokenNumber

		build := func(timeValue string) (string, error) {
			sql, err := fb.StrFTime(args[0].Literal, timeValue, isNumber, modifiers)
			if err != nil {
				return "", fmt.Errorf("[strftime] %w", err)
			}
			return sql, nil
		}

		timeValue := timeValueArgResult.Identifier
		if timeValueArgResult.JSONB {
			timeValue = d.JSONText(timeValue)
		}

		sql, err := build(timeValue)
		if err != nil {
			return nil, err
		}

		result := &ResolverResult{
			NullFallback: NullFallbackEnforced,
			Identifier:   sql,
			Params:       dbx.Params{},
		}
		if err = concatUniqueParams(result.Params, timeValueArgResult.Params); err != nil {
			return nil, err
		}

		if timeValueArgResult.MultiMatchSubQuery != nil {
			mmValue := timeValueArgResult.MultiMatchSubQuery.ValueIdentifier
			if timeValueArgResult.JSONB {
				mmValue = d.JSONText(mmValue)
			}

			mmSQL, err := build(mmValue)
			if err != nil {
				return nil, err
			}

			result.MultiMatchSubQuery = timeValueArgResult.MultiMatchSubQuery
			result.MultiMatchSubQuery.ValueIdentifier = mmSQL

			if err = concatUniqueParams(result.MultiMatchSubQuery.Params, result.Params); err != nil {
				return nil, err
			}
		}

		return result, nil
	},
}

// numericOperand returns a resolved operand as a numeric SQL expression.
func numericOperand(d dialect.Dialect, r *ResolverResult) string {
	if r.JSONB {
		return d.JSONNumber(r.Identifier)
	}

	return "CAST(" + r.Identifier + " AS DOUBLE PRECISION)"
}
