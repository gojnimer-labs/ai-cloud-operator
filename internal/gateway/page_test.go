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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testPageWorkloadName = "my-workload"

func TestRenderLoadingPageIsMinimalAndSelfRefreshing(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/gw/"+testPageWorkloadName+"/http/", nil)
	rec := httptest.NewRecorder()

	renderLoadingPage(rec, req, testPageWorkloadName, 3)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("expected text/html Content-Type, got %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<meta http-equiv="refresh" content="3">`) {
		t.Fatalf("expected a 3-second self-refresh meta tag, got: %s", body)
	}
	if !strings.Contains(body, testPageWorkloadName) {
		t.Fatalf("expected workload name %q in loading page, got: %s", testPageWorkloadName, body)
	}
	// The whole point of simplifying this page: no operator-internal detail
	// (phase strings, replica counts, namespace) leaks into what the user
	// sees while waiting.
	for _, leaked := range []string{"Deploying", "Pending", "replicas", "Namespace"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("expected no %q in the simplified loading page, got: %s", leaked, body)
		}
	}
}

func TestRenderFailedPageShowsMessageAndKeepsRefreshing(t *testing.T) {
	const failureMessage = "reconciling deployment: some underlying error"
	req := httptest.NewRequest(http.MethodGet, "/gw/"+testPageWorkloadName+"/http/", nil)
	rec := httptest.NewRecorder()

	renderFailedPage(rec, req, testPageWorkloadName, failureMessage, 3)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, failureMessage) {
		t.Fatalf("expected failure message %q in failed page, got: %s", failureMessage, body)
	}
	// Failed isn't terminal (the reconciler keeps retrying), so this page
	// must keep polling like the loading page does.
	if !strings.Contains(body, `<meta http-equiv="refresh"`) {
		t.Fatalf("expected self-refreshing meta tag on the failed page, got: %s", body)
	}
}

func TestRenderStoppedPageDoesNotSelfRefresh(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/gw/"+testPageWorkloadName+"/http/", nil)
	rec := httptest.NewRecorder()

	renderStoppedPage(rec, req, testPageWorkloadName)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, testPageWorkloadName) {
		t.Fatalf("expected workload name %q in stopped page, got: %s", testPageWorkloadName, body)
	}
	// Stopped is stable until a user resumes it elsewhere — nothing about it
	// changes on its own, so unlike loading/failed this must not poll.
	if strings.Contains(body, `<meta http-equiv="refresh"`) {
		t.Fatalf("expected no self-refresh meta tag on the stopped page, got: %s", body)
	}
}

func TestRenderNotFoundPageDoesNotSelfRefresh(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/gw/"+testPageWorkloadName+"/http/", nil)
	rec := httptest.NewRecorder()

	renderNotFoundPage(rec, req, testPageWorkloadName)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("expected text/html Content-Type, got %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, testPageWorkloadName) {
		t.Fatalf("expected workload name %q in not-found page, got: %s", testPageWorkloadName, body)
	}
	// A deleted/nonexistent workload won't come back on its own.
	if strings.Contains(body, `<meta http-equiv="refresh"`) {
		t.Fatalf("expected no self-refresh meta tag on the not-found page, got: %s", body)
	}
}

func TestRenderUnauthenticatedPageDoesNotSelfRefresh(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/gw/"+testPageWorkloadName+"/http/", nil)
	rec := httptest.NewRecorder()

	RenderUnauthenticatedPage(rec, req, testPageWorkloadName)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("expected text/html Content-Type, got %q", ct)
	}
	body := rec.Body.String()
	// Nothing about an expired/invalid link resolves itself, unlike a
	// workload that's still starting up or recovering from a failure.
	if strings.Contains(body, `<meta http-equiv="refresh"`) {
		t.Fatalf("expected no self-refresh meta tag on the unauthenticated page, got: %s", body)
	}
	if !strings.Contains(body, testPageWorkloadName) {
		t.Fatalf("expected workload name %q in unauthenticated page, got: %s", testPageWorkloadName, body)
	}
}
