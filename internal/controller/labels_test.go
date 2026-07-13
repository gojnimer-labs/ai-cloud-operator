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

package controller

import (
	"strings"
	"testing"
)

func TestSanitizeLabelValue(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantOK  bool
		wantVal string
	}{
		{"empty", "", false, ""},
		{"simple alnum", "user123", true, "user123"},
		{"with dash underscore dot", "user-1_2.3", true, "user-1_2.3"},
		{"starts with dash", "-user", false, ""},
		{"ends with dot", "user.", false, ""},
		{"too long", strings.Repeat("a", 64), false, ""},
		{"max length ok", strings.Repeat("a", 63), true, strings.Repeat("a", 63)},
		{"invalid char", "user@id", false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sanitizeLabelValue(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.wantVal {
				t.Fatalf("got %q, want %q", got, tc.wantVal)
			}
		})
	}
}
