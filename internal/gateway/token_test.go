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
	"testing"
	"time"
)

const testSecret = "test-gateway-signing-secret"

func mintTestToken(t *testing.T, secret string, p Payload) string {
	t.Helper()
	token, err := Sign([]byte(secret), p)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return token
}

func TestVerifyValidTokenRoundTrips(t *testing.T) {
	token := mintTestToken(t, testSecret, Payload{
		Namespace: "default",
		Name:      "demo",
		UserID:    "user-1",
		Exp:       time.Now().Add(time.Minute).Unix(),
	})

	p, err := Verify([]byte(testSecret), "default", "demo", token)
	if err != nil {
		t.Fatalf("expected valid token, got %v", err)
	}
	if p.UserID != "user-1" {
		t.Fatalf("expected userId user-1, got %q", p.UserID)
	}
}

func TestVerifyRejectsTamperedSignature(t *testing.T) {
	token := mintTestToken(t, testSecret, Payload{
		Namespace: "default",
		Name:      "demo",
		Exp:       time.Now().Add(time.Minute).Unix(),
	})
	tampered := token[:len(token)-1] + "x"

	if _, err := Verify([]byte(testSecret), "default", "demo", tampered); err != ErrInvalidSignature {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	token := mintTestToken(t, testSecret, Payload{
		Namespace: "default",
		Name:      "demo",
		Exp:       time.Now().Add(time.Minute).Unix(),
	})

	if _, err := Verify([]byte("wrong-secret"), "default", "demo", token); err != ErrInvalidSignature {
		t.Fatalf("expected ErrInvalidSignature, got %v", err)
	}
}

func TestVerifyRejectsExpiredToken(t *testing.T) {
	token := mintTestToken(t, testSecret, Payload{
		Namespace: "default",
		Name:      "demo",
		Exp:       time.Now().Add(-time.Hour).Unix(),
	})

	if _, err := Verify([]byte(testSecret), "default", "demo", token); err != ErrExpired {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
}

func TestVerifyToleratesSmallClockSkew(t *testing.T) {
	token := mintTestToken(t, testSecret, Payload{
		Namespace: "default",
		Name:      "demo",
		Exp:       time.Now().Add(-10 * time.Second).Unix(),
	})

	if _, err := Verify([]byte(testSecret), "default", "demo", token); err != nil {
		t.Fatalf("expected token within clock-skew tolerance to verify, got %v", err)
	}
}

func TestVerifyRejectsWrongWorkloadScope(t *testing.T) {
	token := mintTestToken(t, testSecret, Payload{
		Namespace: "default",
		Name:      "workload-a",
		Exp:       time.Now().Add(time.Minute).Unix(),
	})

	if _, err := Verify([]byte(testSecret), "default", "workload-b", token); err != ErrScopeMismatch {
		t.Fatalf("expected ErrScopeMismatch, got %v", err)
	}
}

func TestVerifyRejectsMalformedToken(t *testing.T) {
	for _, tok := range []string{"", "no-dot-here", ".", "abc.", ".abc"} {
		if _, err := Verify([]byte(testSecret), "default", "demo", tok); err == nil {
			t.Fatalf("expected an error for malformed token %q", tok)
		}
	}
}
