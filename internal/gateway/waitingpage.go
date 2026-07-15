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

//go:embed waitingpage.html
var waitingPageFS embed.FS

var waitingPageTmpl = template.Must(template.ParseFS(waitingPageFS, "waitingpage.html"))

// waitingPageData is the waitingpage.html template's full field list.
type waitingPageData struct {
	Namespace       string
	Name            string
	Phase           string // "" (not yet reconciled), "Deploying", or "Failed"
	Message         string // Ready condition's Message, if any; "" is valid
	ReadyReplicas   int32
	DesiredReplicas int32
	RefreshSeconds  int
	ShowRefresh     bool
	Failed          bool // selects error styling/copy vs. the "starting up" spinner
}

// renderWaitingPage renders waitingpage.html with data into a buffer first —
// so a template execution error becomes a clean 500 instead of a
// half-written 200 HTML body — then writes statusCode and the buffered body.
func renderWaitingPage(w http.ResponseWriter, r *http.Request, statusCode int, data waitingPageData) {
	var buf bytes.Buffer
	if err := waitingPageTmpl.Execute(&buf, data); err != nil {
		logf.FromContext(r.Context()).WithName("gateway").Error(err, "rendering waiting page")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(statusCode)
	_, _ = w.Write(buf.Bytes())
}
