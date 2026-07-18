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
	"bytes"
	"embed"
	"html/template"
	"net/http"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

//go:embed page.html
var pageFS embed.FS

var pageTmpl = template.Must(template.ParseFS(pageFS, "page.html"))

// pageKind selects which of page.html's three variants to render.
type pageKind string

const (
	pageLoading         pageKind = "loading"
	pageFailed          pageKind = "failed"
	pageStopped         pageKind = "stopped"
	pageNotFound        pageKind = "notfound"
	pageUnauthenticated pageKind = "unauthenticated"
)

// pageData is page.html's full field list. Deliberately minimal — no
// namespace, phase string, or replica counts — see renderLoadingPage's doc
// comment for why that operator-internal detail was dropped from what an
// end user actually sees.
type pageData struct {
	Kind pageKind
	Name string
	// Message is only meaningful for pageFailed (the Ready condition's
	// message); "" is valid.
	Message        string
	RefreshSeconds int
	ShowRefresh    bool
}

// renderPage renders page.html with data into a buffer first — so a
// template execution error becomes a clean 500 instead of a half-written
// 200 HTML body — then writes statusCode and the buffered body.
func renderPage(w http.ResponseWriter, r *http.Request, statusCode int, data pageData) {
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, data); err != nil {
		logf.FromContext(r.Context()).WithName("gateway").Error(err, "rendering gateway page")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(statusCode)
	_, _ = w.Write(buf.Bytes())
}

// renderLoadingPage shows a minimal "starting up" screen while a workload's
// Deployment is still progressing toward Ready. Deliberately light on
// detail — no phase string, no replica counts, no namespace — so a user
// waiting for their workspace sees a clean loading screen instead of
// operator-internal reconciliation state; refreshSeconds still polls in the
// background until the workload becomes reachable.
func renderLoadingPage(w http.ResponseWriter, r *http.Request, name string, refreshSeconds int) {
	renderPage(w, r, http.StatusOK, pageData{
		Kind:           pageLoading,
		Name:           name,
		RefreshSeconds: refreshSeconds,
		ShowRefresh:    true,
	})
}

// renderFailedPage shows a workload's Ready-condition failure message.
// Still self-refreshing: setFailed's requeue means Failed isn't a terminal
// state, so a transient failure can recover on its own without the user
// having to reload manually.
func renderFailedPage(w http.ResponseWriter, r *http.Request, name, message string, refreshSeconds int) {
	renderPage(w, r, http.StatusServiceUnavailable, pageData{
		Kind:           pageFailed,
		Name:           name,
		Message:        message,
		RefreshSeconds: refreshSeconds,
		ShowRefresh:    true,
	})
}

// renderStoppedPage shows that a workload is intentionally suspended
// (Spec.Suspended=true, Status.Phase=Stopped) rather than starting up or
// broken. Unlike loading/failed, this is a stable, terminal-until-resumed
// state — nothing about it changes on its own, so like
// RenderUnauthenticatedPage it deliberately does not self-refresh; polling
// every few seconds while a user has to go elsewhere to resume it would
// just be noise.
func renderStoppedPage(w http.ResponseWriter, r *http.Request, name string) {
	renderPage(w, r, http.StatusOK, pageData{
		Kind: pageStopped,
		Name: name,
	})
}

// renderNotFoundPage shows that no Workload exists by this name — deleted,
// never existed, or a mistyped link. Distinct from renderStoppedPage even
// though both are terminal/non-self-refreshing: this is Handler's very
// first lookup, before there's any Workload object to read a phase from at
// all, so it has no Ready-condition message or phase to show — just the
// name from the URL itself.
func renderNotFoundPage(w http.ResponseWriter, r *http.Request, name string) {
	renderPage(w, r, http.StatusNotFound, pageData{
		Kind: pageNotFound,
		Name: name,
	})
}

// RenderUnauthenticatedPage shows a browser-friendly explanation for a
// gateway request that failed authentication (missing, invalid, expired, or
// already-consumed token/cookie) — exported for internal/api.Server's
// requireGatewayToken to call, since that's where every one of those
// rejections is actually decided. Not self-refreshing: unlike a workload
// that's still starting up or recovering, nothing about this state changes
// on its own — the caller needs a fresh link.
func RenderUnauthenticatedPage(w http.ResponseWriter, r *http.Request, name string) {
	renderPage(w, r, http.StatusUnauthorized, pageData{
		Kind: pageUnauthenticated,
		Name: name,
	})
}
