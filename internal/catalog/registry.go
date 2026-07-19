/*
Copyright 2026 gojnimer-labs.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
)

// templates is the fixed registry of available workload templates.
var templates = []Template{
	Nginx,
	Firefox,
	Chrome,
}

// List returns all catalog templates, in a stable order.
func List() []Template {
	return templates
}

// Hash returns a deterministic fingerprint of the full catalog — sha256 of
// its JSON encoding (the same wire-relevant fields GET /catalog and
// operator registration already send; Build/Run are excluded via their
// json:"-" tags). internal/convexclient.Runnable persists this alongside
// its registration tokens and compares it at startup to detect that this
// operator's own catalog changed (e.g. a Template.Version bump) since it
// last registered with Convex, since a token Secret surviving a restart
// would otherwise mask that indefinitely.
func Hash() string {
	data, _ := json.Marshal(templates)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Get looks up a template by ID.
func Get(id string) (Template, bool) {
	for _, t := range templates {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

// GetOperation looks up a named Operation on a template.
func GetOperation(t Template, key string) (Operation, bool) {
	for _, op := range t.Operations {
		if op.Key == key {
			return op, true
		}
	}
	return Operation{}, false
}

// ResolveParams applies each parameter's default (when the caller didn't
// supply a value), then checks Required/Validation on every parameter that's
// currently visible. It returns a new map — raw is never mutated. Takes a
// bare parameter list rather than a Template so it's equally usable for a
// Template's own Parameters and an Operation's Parameters.
//
// Two passes, deliberately: a Visibility condition can depend on another
// parameter's *resolved* value (including one that only got a value from its
// own Default), so every default must already be applied before any
// Visibility can be evaluated. A parameter that resolves as not-visible is
// exempt from Required and Validation entirely — nothing rendered a form
// field for it, so demanding a value would be unsatisfiable by construction.
// Its value (if any slipped in via raw config anyway) is left untouched, not
// stripped — Build functions may still read it defensively.
func ResolveParams(params []Parameter, raw map[string]any) (map[string]any, error) {
	resolved := make(map[string]any, len(params))
	maps.Copy(resolved, raw)

	for _, p := range params {
		if _, ok := resolved[p.Key]; !ok && p.Default != nil {
			resolved[p.Key] = p.Default
		}
	}

	for _, p := range params {
		if p.Visibility != nil && !evalVisibility(p.Visibility, resolved[p.Visibility.DependsOn]) {
			continue
		}

		v, ok := resolved[p.Key]
		present := ok && v != nil && v != ""
		if p.Validation.Required && !present {
			return nil, fmt.Errorf("missing required parameter %q", p.Key)
		}
		if present {
			if err := checkValidation(&p.Validation, v); err != nil {
				return nil, fmt.Errorf("parameter %q invalid: %w", p.Key, err)
			}
		}
	}
	return resolved, nil
}

// evalVisibility reports whether a parameter carrying this condition should
// currently be treated as visible, given the depended-on parameter's
// resolved value.
func evalVisibility(v *Visibility, actual any) bool {
	switch v.Op {
	case VisibilityEquals:
		return actual == v.Value
	case VisibilityNotEquals:
		return actual != v.Value
	case VisibilityOneOf:
		return slices.Contains(v.Values, actual)
	default:
		return true
	}
}

// checkValidation applies a Validation's constraints to a present value.
// Constraints that don't apply to value's actual type (e.g. Min/Max against
// a string) are silently skipped rather than treated as a mismatch error —
// a template author declaring both a numeric and string constraint on the
// same parameter would be a template bug, not something a caller's input
// should be blamed for.
func checkValidation(rule *Validation, value any) error {
	if rule.Min != nil || rule.Max != nil {
		if num, ok := asFloat64(value); ok {
			if rule.Min != nil && num < *rule.Min {
				return fmt.Errorf("must be >= %g", *rule.Min)
			}
			if rule.Max != nil && num > *rule.Max {
				return fmt.Errorf("must be <= %g", *rule.Max)
			}
		}
	}
	if rule.MaxLength != nil || rule.Regex != "" {
		if s, ok := value.(string); ok {
			if rule.MaxLength != nil && len(s) > *rule.MaxLength {
				return fmt.Errorf("must be at most %d characters", *rule.MaxLength)
			}
			if rule.Regex != "" {
				matched, err := regexp.MatchString(rule.Regex, s)
				if err != nil {
					return fmt.Errorf("invalid validation regex %q: %w", rule.Regex, err)
				}
				if !matched {
					return fmt.Errorf("must match pattern %q", rule.Regex)
				}
			}
		}
	}
	return nil
}

// asFloat64 reads a numeric value regardless of whether it arrived as a
// JSON-decoded float64 or a literal int/int32 (e.g. from a test constructing
// params directly) — same reasoning as paramInt32 below.
func asFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	default:
		return 0, false
	}
}

// ptrFloat64 takes the address of a float64 literal — Validation.Min/Max are
// pointers (so "no constraint" and "constrained to 0" are distinguishable),
// which Go struct literals can't populate from a bare numeric literal.
func ptrFloat64(v float64) *float64 {
	return &v
}

func paramString(params map[string]any, key, fallback string) string {
	if v, ok := params[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return fallback
}

// paramInt32 reads a numeric parameter. Values decoded from JSON into `any`
// arrive as float64, so that's the primary case; a literal int/int32 is
// accepted too for callers constructing params directly (e.g. tests).
func paramInt32(params map[string]any, key string, fallback int32) int32 {
	switch v := params[key].(type) {
	case float64:
		return int32(v)
	case int32:
		return v
	case int:
		return int32(v)
	default:
		return fallback
	}
}

func int32ToString(v int32) string {
	return strconv.FormatInt(int64(v), 10)
}
