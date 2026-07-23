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

package gateway

import (
	"strings"
	"testing"
	"time"
)

const (
	testSecret = "test-gateway-signing-secret"
	namespace  = "default"
	name       = "demo"
)

func mintTestToken(t *testing.T, p Payload) string {
	t.Helper()
	token, err := Sign([]byte(testSecret), p)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return token
}

func TestVerifyValidTokenRoundTrips(t *testing.T) {
	token := mintTestToken(t, Payload{
		Namespace: namespace,
		Name:      name,
		UserID:    "user-1",
		Exp:       time.Now().Add(time.Minute).Unix(),
	})

	p, err := Verify([]byte(testSecret), namespace, name, token)
	if err != nil {
		t.Fatalf("expected valid token, got %v", err)
	}
	if p.UserID != "user-1" {
		t.Fatalf("expected userId user-1, got %q", p.UserID)
	}
}

func TestVerifyRejectsTamperedSignature(t *testing.T) {
	token := mintTestToken(t, Payload{
		Namespace: namespace,
		Name:      name,
		Exp:       time.Now().Add(time.Minute).Unix(),
	})
	// Flip the signature segment's *first* character, not the token's last
	// character. SHA-256 is 32 bytes, not a multiple of 3, so
	// base64.RawURLEncoding's final character of that segment only carries
	// 4 significant bits — several different characters there decode to
	// the identical byte, so even a guaranteed-different *character* can
	// be a no-op at the decoded-*byte* level (this is what made the test
	// flaky: ~1/64 of runs picked a "different" char that still decoded
	// the same). A character well inside a full 3-byte group has no such
	// ambiguity — any different character there unambiguously changes the
	// decoded bytes.
	sigStart := strings.IndexByte(token, '.') + 1
	replacement := byte('x')
	if token[sigStart] == replacement {
		replacement = 'y'
	}
	tampered := token[:sigStart] + string(replacement) + token[sigStart+1:]

	if _, err := Verify([]byte(testSecret), namespace, name, tampered); err != ErrInvalidSignature {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	token := mintTestToken(t, Payload{
		Namespace: namespace,
		Name:      name,
		Exp:       time.Now().Add(time.Minute).Unix(),
	})

	if _, err := Verify([]byte("wrong-secret"), namespace, name, token); err != ErrInvalidSignature {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}
}

func TestVerifyRejectsExpiredToken(t *testing.T) {
	token := mintTestToken(t, Payload{
		Namespace: namespace,
		Name:      name,
		Exp:       time.Now().Add(-time.Hour).Unix(),
	})

	if _, err := Verify([]byte(testSecret), namespace, name, token); err != ErrExpired {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
}

func TestVerifyToleratesSmallClockSkew(t *testing.T) {
	token := mintTestToken(t, Payload{
		Namespace: namespace,
		Name:      name,
		Exp:       time.Now().Add(-10 * time.Second).Unix(),
	})

	if _, err := Verify([]byte(testSecret), namespace, name, token); err != nil {
		t.Fatalf("expected token within clock-skew tolerance to verify, got %v", err)
	}
}

func TestVerifyRejectsWrongWorkloadScope(t *testing.T) {
	token := mintTestToken(t, Payload{
		Namespace: namespace,
		Name:      "workload-a",
		Exp:       time.Now().Add(time.Minute).Unix(),
	})

	if _, err := Verify([]byte(testSecret), namespace, "workload-b", token); err != ErrScopeMismatch {
		t.Fatalf("expected ErrScopeMismatch, got %v", err)
	}
}

func TestVerifyRejectsMalformedToken(t *testing.T) {
	for _, tok := range []string{"", "no-dot-here", ".", "abc.", ".abc"} {
		if _, err := Verify([]byte(testSecret), namespace, name, tok); err == nil {
			t.Fatalf("expected an error for malformed token %q", tok)
		}
	}
}
