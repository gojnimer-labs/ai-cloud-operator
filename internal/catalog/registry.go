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
	"fmt"
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

// Get looks up a template by ID.
func Get(id string) (Template, bool) {
	for _, t := range templates {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

// ResolveParams applies each parameter's default (when the caller didn't
// supply a value) and checks that every required parameter ends up with a
// value. It returns a new map — raw is never mutated.
func ResolveParams(t Template, raw map[string]any) (map[string]any, error) {
	resolved := make(map[string]any, len(t.Parameters))
	for k, v := range raw {
		resolved[k] = v
	}

	for _, p := range t.Parameters {
		if _, ok := resolved[p.Key]; !ok && p.Default != nil {
			resolved[p.Key] = p.Default
		}
		if p.Required {
			if v, ok := resolved[p.Key]; !ok || v == nil || v == "" {
				return nil, fmt.Errorf("missing required parameter %q", p.Key)
			}
		}
	}
	return resolved, nil
}

func paramString(params map[string]any, key, fallback string) string {
	if v, ok := params[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return fallback
}

func paramBool(params map[string]any, key string, fallback bool) bool {
	if v, ok := params[key]; ok {
		if b, ok := v.(bool); ok {
			return b
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
