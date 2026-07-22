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

// Package convexclient talks to Convex's operator registration/heartbeat
// HTTP endpoints (convex/operators/http.ts in ai-cloud-v2).
package convexclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gojnimer-labs/ai-cloud-operator/internal/capacity"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/catalog"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/metrics"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/tokenstore"
)

// ErrUnauthorized indicates Convex rejected the presented credential (401 or
// 410) — the caller should treat this as "re-register".
var ErrUnauthorized = fmt.Errorf("convex rejected credential")

// ErrLifecycleStale indicates ReportLifecycle's 409: Convex's row for this
// workload isn't in an in-flight status right now, so this report can never
// be applied as-is. Unlike a network blip or a 5xx, retrying the exact same
// call will never succeed — the row only re-enters an in-flight status via
// a fresh redeploy/stop/resume, which reports lifecycle again on its own.
// The caller should treat this as "nothing further to do this generation,"
// not as a transient failure worth requeuing over (see
// WorkloadReconciler.syncConvexLifecyclePhase).
var ErrLifecycleStale = fmt.Errorf("workload not in an in-flight status")

// Config holds the operator's identity and how to reach Convex.
type Config struct {
	BaseURL          string
	EnrollmentSecret string
	OperatorName     string
	ExternalURL      string
	Metadata         map[string]any
	// Version is this operator build's own version string (see cmd/main.go's
	// Version var, set via -ldflags at build time). Reported on every
	// Register call so Convex's fleet table can display it — never gates
	// anything server-side.
	Version string
	// Tags are this operator's user-configured tags (OPERATOR_TAGS env var,
	// see cmd/main.go), reported on every Register call. Once Convex sees a
	// non-nil Tags on a register request — even an empty slice — it locks
	// that operator's tags against further edits from the admin UI; only a
	// fresh Register call can change them again (convex/operators/
	// mutations.ts's claim/updateCluster). A nil Tags here means "this
	// install never set OPERATOR_TAGS," which is intentionally
	// indistinguishable from "not reported" so tags admin-set through
	// Convex directly aren't clobbered on every restart.
	Tags []string
}

// Client is a thin HTTP client for the two Convex operator endpoints.
type Client struct {
	config     Config
	httpClient *http.Client
}

// New returns a Client configured to reach Convex at cfg.BaseURL.
func New(cfg Config) *Client {
	return &Client{
		config:     cfg,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// EnrollmentSecret returns the enrollment secret this Client currently
// registers with.
func (c *Client) EnrollmentSecret() string {
	return c.config.EnrollmentSecret
}

// SetEnrollmentSecret updates the enrollment secret used by future calls to
// Register — e.g. after Runnable detects the backing Secret was rotated.
// Only safe to call from the same goroutine that drives Runnable.Start, same
// as the rest of Client's state.
func (c *Client) SetEnrollmentSecret(secret string) {
	c.config.EnrollmentSecret = secret
}

type registerRequest struct {
	Name             string         `json:"name"`
	ExternalURL      string         `json:"externalUrl"`
	EnrollmentSecret string         `json:"enrollmentSecret"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	// Catalog is this operator's full template registry (internal/catalog.List())
	// — Convex persists it (operators.catalog/catalogReportedAt) and uses it
	// at claim time to verify the claiming operator actually supports the
	// exact templateId+templateVersion a workload row was requested against.
	Catalog []catalog.Template `json:"catalog"`
	// OperatorVersion is display-only (see Config.Version's doc comment).
	OperatorVersion string `json:"operatorVersion,omitempty"`
	// Tags is a pointer, not a plain slice: encoding/json's omitempty on a
	// plain slice drops it whenever len == 0, which would silently discard
	// an explicit "report zero tags" the same as "OPERATOR_TAGS was never
	// set" — but Convex's claim mutation treats those as different signals
	// (see Config.Tags's doc comment). omitempty on a pointer instead keys
	// off nil-ness alone, so a non-nil pointer to an empty slice still
	// marshals as "tags":[] while a nil pointer omits the field entirely.
	Tags *[]string `json:"tags,omitempty"`
}

type registerResponse struct {
	HeartbeatToken string `json:"heartbeatToken"`
	DeployToken    string `json:"deployToken"`
}

// Register performs one-time enrollment with Convex using the pre-shared
// enrollment secret, returning the pair of tokens Convex minted for this
// operator.
func (c *Client) Register(ctx context.Context) (tokenstore.Tokens, error) {
	var tags *[]string
	if c.config.Tags != nil {
		tags = &c.config.Tags
	}

	body, err := json.Marshal(registerRequest{
		Name:             c.config.OperatorName,
		ExternalURL:      c.config.ExternalURL,
		EnrollmentSecret: c.config.EnrollmentSecret,
		Metadata:         c.config.Metadata,
		Catalog:          catalog.List(),
		OperatorVersion:  c.config.Version,
		Tags:             tags,
	})
	if err != nil {
		return tokenstore.Tokens{}, fmt.Errorf("marshaling register request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL+"/operators/register", bytes.NewReader(body))
	if err != nil {
		return tokenstore.Tokens{}, fmt.Errorf("building register request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return tokenstore.Tokens{}, fmt.Errorf("calling register: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return tokenstore.Tokens{}, fmt.Errorf("register returned status %d", resp.StatusCode)
	}

	var out registerResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return tokenstore.Tokens{}, fmt.Errorf("decoding register response: %w", err)
	}

	return tokenstore.Tokens{HeartbeatToken: out.HeartbeatToken, DeployToken: out.DeployToken}, nil
}

type heartbeatRequest struct {
	Name string `json:"name"`
	// ResourceCapacity is report-only, for the admin fleet-visibility view
	// (see convex/operators/mutations.ts#markHeartbeat in ai-cloud-v2) — it
	// is never read by any claim/listClaimable gating logic. omitempty so an
	// unconfigured Tracker (nil snapshot) leaves Convex's prior value
	// untouched rather than overwriting it with zeros.
	ResourceCapacity *resourceCapacityWire `json:"resourceCapacity,omitempty"`
}

// resourceCapacityWire mirrors capacity.Snapshot's fields for the wire —
// kept as a separate type (rather than reusing capacity.Snapshot directly)
// so this package's JSON contract with Convex doesn't silently change shape
// if capacity.Snapshot ever gains an internal-only field.
type resourceCapacityWire struct {
	AllocatableMilliCPU    int64 `json:"allocatableMilliCpu"`
	AllocatableMemoryBytes int64 `json:"allocatableMemoryBytes"`
	UsedMilliCPU           int64 `json:"usedMilliCpu"`
	UsedMemoryBytes        int64 `json:"usedMemoryBytes"`
}

// claimableWorkloadWire/pendingOperationWire are heartbeatResponse's list
// element shapes, kept private since callers get the public
// ClaimableWorkload/PendingOperation instead (see Heartbeat).
type claimableWorkloadWire struct {
	WorkloadID string `json:"workloadId"`
	TemplateID string `json:"templateId"`
}

type pendingOperationWire struct {
	WorkloadID string `json:"workloadId"`
	Operation  string `json:"operation"`
}

type heartbeatResponse struct {
	Claimable         []claimableWorkloadWire `json:"claimable"`
	PendingOperations []pendingOperationWire  `json:"pendingOperations"`
}

// ClaimableWorkload is one brand-new, not-yet-assigned workload this
// operator's tags qualify it to claim — see Client.ClaimWorkload.
type ClaimableWorkload struct {
	WorkloadID string
	TemplateID string
}

// PendingOperation is one destroy/redeploy request already assigned to this
// operator (from a prior create), waiting to be claimed — see
// Client.ClaimOperation. Operation is "destroy" or "redeploy".
type PendingOperation struct {
	WorkloadID string
	Operation  string
}

// Heartbeat reports liveness to Convex using the previously issued heartbeat
// token, and returns the work this operator can currently pick up: brand-new
// requests matching its tags (claimable) and destroy/redeploy requests
// already assigned to it (pendingOperations) — see
// internal/convexclient/runnable.go's claim-consumption loop for how both
// get processed. Returns ErrUnauthorized if Convex rejects the token
// (401/410), signaling the caller should discard it and re-register.
//
// snapshot, when non-nil, is included purely for Convex's admin
// fleet-visibility view — nil (no Tracker configured, or this tick's
// Snapshot call errored) omits it from the request entirely rather than
// sending zeros.
func (c *Client) Heartbeat(ctx context.Context, heartbeatToken string, snapshot *capacity.Snapshot) ([]ClaimableWorkload, []PendingOperation, error) {
	var resourceCapacity *resourceCapacityWire
	if snapshot != nil {
		resourceCapacity = &resourceCapacityWire{
			AllocatableMilliCPU:    snapshot.AllocatableMilliCPU,
			AllocatableMemoryBytes: snapshot.AllocatableMemoryBytes,
			UsedMilliCPU:           snapshot.UsedMilliCPU,
			UsedMemoryBytes:        snapshot.UsedMemoryBytes,
		}
	}
	body, err := json.Marshal(heartbeatRequest{Name: c.config.OperatorName, ResourceCapacity: resourceCapacity})
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling heartbeat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL+"/operators/heartbeat", bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("building heartbeat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+heartbeatToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("calling heartbeat: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var out heartbeatResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, nil, fmt.Errorf("decoding heartbeat response: %w", err)
		}
		claimable := make([]ClaimableWorkload, len(out.Claimable))
		for i, w := range out.Claimable {
			claimable[i] = ClaimableWorkload(w)
		}
		pending := make([]PendingOperation, len(out.PendingOperations))
		for i, p := range out.PendingOperations {
			pending[i] = PendingOperation(p)
		}
		return claimable, pending, nil
	case http.StatusUnauthorized, http.StatusGone:
		return nil, nil, ErrUnauthorized
	default:
		return nil, nil, fmt.Errorf("heartbeat returned status %d", resp.StatusCode)
	}
}

// ClaimedWorkload is what a successful ClaimWorkload call hands back — the
// info WorkloadCreator.Create needs to actually build the CR. WorkloadID
// echoes back the correlation token (the Convex row's own _id) that Create
// stamps onto the CR as a label, since the CR's real name doesn't exist yet
// (still minted via GenerateName).
type ClaimedWorkload struct {
	WorkloadID      string
	Config          map[string]any
	Subdomain       string
	TemplateID      string
	TemplateVersion string
	UserID          string
}

type claimWorkloadRequest struct {
	WorkloadID string `json:"workloadId"`
}

type claimWorkloadResponse struct {
	WorkloadID      string         `json:"workloadId"`
	Config          map[string]any `json:"config,omitempty"`
	Subdomain       string         `json:"subdomain,omitempty"`
	TemplateID      string         `json:"templateId"`
	TemplateVersion string         `json:"templateVersion,omitempty"`
	UserID          string         `json:"userId"`
}

// ClaimWorkload attempts to claim a brand-new workload request previously
// surfaced by Heartbeat's claimable list. Returns (nil, nil) — not an error
// — when Convex reports the claim didn't land (404/409): another operator
// already claimed it, or it's no longer in a claimable state. The caller
// should simply skip it, not treat this as a failure.
func (c *Client) ClaimWorkload(ctx context.Context, heartbeatToken, workloadID string) (*ClaimedWorkload, error) {
	body, err := json.Marshal(claimWorkloadRequest{WorkloadID: workloadID})
	if err != nil {
		return nil, fmt.Errorf("marshaling claim workload request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL+"/operators/workloads/claim", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building claim workload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+heartbeatToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling claim workload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var out claimWorkloadResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("decoding claim workload response: %w", err)
		}
		return &ClaimedWorkload{
			WorkloadID:      out.WorkloadID,
			Config:          out.Config,
			Subdomain:       out.Subdomain,
			TemplateID:      out.TemplateID,
			TemplateVersion: out.TemplateVersion,
			UserID:          out.UserID,
		}, nil
	case http.StatusNotFound, http.StatusConflict:
		return nil, nil
	default:
		return nil, fmt.Errorf("claim workload returned status %d", resp.StatusCode)
	}
}

// ClaimedOperation is what a successful ClaimOperation call hands back.
// Config/TemplateID/TemplateVersion are only populated when Operation ==
// "redeploy" — a "destroy" only ever needs Name/Namespace, both of which are
// already known from the original create-time upsert.
type ClaimedOperation struct {
	Operation       string
	Name            string
	Namespace       string
	Config          map[string]any
	TemplateID      string
	TemplateVersion string
}

type claimOperationRequest struct {
	WorkloadID string `json:"workloadId"`
}

type claimOperationResponse struct {
	Operation       string         `json:"operation"`
	Name            string         `json:"name"`
	Namespace       string         `json:"namespace"`
	Config          map[string]any `json:"config,omitempty"`
	TemplateID      string         `json:"templateId,omitempty"`
	TemplateVersion string         `json:"templateVersion,omitempty"`
}

// ClaimOperation attempts to claim a pending destroy/redeploy request
// previously surfaced by Heartbeat's pendingOperations list. Returns (nil,
// nil) — not an error — when Convex reports the claim didn't land (404/409):
// lost a race with another attempt (shouldn't normally happen, since these
// are already scoped to this operator), or it's no longer in a claimable
// state.
func (c *Client) ClaimOperation(ctx context.Context, heartbeatToken, workloadID string) (*ClaimedOperation, error) {
	body, err := json.Marshal(claimOperationRequest{WorkloadID: workloadID})
	if err != nil {
		return nil, fmt.Errorf("marshaling claim operation request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL+"/operators/workloads/claim-operation", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building claim operation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+heartbeatToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling claim operation: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var out claimOperationResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("decoding claim operation response: %w", err)
		}
		return &ClaimedOperation{
			Operation:       out.Operation,
			Name:            out.Name,
			Namespace:       out.Namespace,
			Config:          out.Config,
			TemplateID:      out.TemplateID,
			TemplateVersion: out.TemplateVersion,
		}, nil
	case http.StatusNotFound, http.StatusConflict:
		return nil, nil
	default:
		return nil, fmt.Errorf("claim operation returned status %d", resp.StatusCode)
	}
}

type reportLifecycleRequest struct {
	// Name is the Workload CR's real k8s name — known for every case except
	// a create attempt that fails (template-version mismatch, or the Create
	// call itself erroring) before the CR exists to have one. WorkloadID
	// covers exactly that gap: Convex's reportLifecycle looks a row up by
	// (operatorId, name) when Name is set, falling back to a direct
	// workloadId lookup when it's the only identifier available. Passing
	// both when both are known is harmless — Convex only needs one to
	// resolve the row.
	//
	// NOTE for the Convex-side implementer: this is a deliberate deviation
	// from the plan's original name-only sketch (A8/B1) — see this repo's
	// implementation report for why a name-only request can never resolve a
	// pre-Create-failure or pre-first-upsert row.
	Name       string `json:"name,omitempty"`
	WorkloadID string `json:"workloadId,omitempty"`
	Phase      string `json:"phase"`
	Reason     string `json:"reason,omitempty"`
	// Retryable marks a "failed" report as transient: Convex releases the
	// claim back to a queued state for a future retry (see
	// convex/workloads/mutations.ts#releaseClaim in ai-cloud-v2) instead of
	// applying the plain phase-resolution path. MUST be true whenever phase
	// is reported against a "destroying" row — Convex has no non-retryable
	// resolution for that status (destroy completion is normally reported
	// via the separate RemoveWorkload call, never through here).
	Retryable bool `json:"retryable,omitempty"`
}

// ReportLifecycle tells Convex a claimed create/redeploy/stop/resume/destroy
// attempt reached an outcome. phase "active"/"stopped" always means success;
// phase "failed" means it didn't, with reason explaining why and retryable
// indicating whether Convex should release the claim for another attempt
// (see reportLifecycleRequest.Retryable) rather than resolve it as a
// terminal-for-now state. Safe to call unconditionally for any CR, including
// manual/legacy ones with no in-flight Convex row: Convex's reportLifecycle
// mutation is a no-op unless the row it resolves is actually in one of the
// in-flight statuses. At least one of name/workloadID should be non-empty;
// see reportLifecycleRequest for why both are accepted.
func (c *Client) ReportLifecycle(ctx context.Context, heartbeatToken, name, workloadID, phase, reason string, retryable bool) error {
	body, err := json.Marshal(reportLifecycleRequest{Name: name, WorkloadID: workloadID, Phase: phase, Reason: reason, Retryable: retryable})
	if err != nil {
		return fmt.Errorf("marshaling report lifecycle request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL+"/operators/workloads/lifecycle", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building report lifecycle request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+heartbeatToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling report lifecycle: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusConflict {
		return ErrLifecycleStale
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("report lifecycle returned status %d", resp.StatusCode)
	}
	return nil
}

// WorkloadInfo is the ownership/identity data the reconciler reports back to
// Convex — deliberately just the fields already on Workload.Spec, never
// runtime status/phase (that stays fetched live, never mirrored). WorkloadID
// is only ever non-empty for a claim-flow-created CR (read off the
// apps.aicloud.dev/workload-id label — see internal/labels) — Convex uses it
// for the direct-by-_id lookup that turns a "provisioning" row (which has no
// name yet) into one with this workload's real generated name.
type WorkloadInfo struct {
	Name         string
	Namespace    string
	TemplateName string
	UserID       string
	Subdomain    string
	WorkloadID   string
}

type upsertWorkloadRequest struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	TemplateID string `json:"templateId"`
	UserID     string `json:"userId"`
	Subdomain  string `json:"subdomain,omitempty"`
	WorkloadID string `json:"workloadId,omitempty"`
}

// UpsertWorkload tells Convex this workload currently exists with this
// ownership info. Called by the reconciler after a successful reconcile of
// a newly-created or spec-changed Workload — see internal/controller.
func (c *Client) UpsertWorkload(ctx context.Context, heartbeatToken string, info WorkloadInfo) error {
	body, err := json.Marshal(upsertWorkloadRequest{
		Name:       info.Name,
		Namespace:  info.Namespace,
		TemplateID: info.TemplateName,
		UserID:     info.UserID,
		Subdomain:  info.Subdomain,
		WorkloadID: info.WorkloadID,
	})
	if err != nil {
		return fmt.Errorf("marshaling upsert workload request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL+"/operators/workloads/upsert", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building upsert workload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+heartbeatToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling upsert workload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upsert workload returned status %d", resp.StatusCode)
	}
	return nil
}

type verifyGatewayTokenRequest struct {
	Token     string `json:"token"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type verifyGatewayTokenResponse struct {
	UserID string `json:"userId"`
}

// VerifyGatewayToken asks Convex to check and consume a one-time gateway
// access token minted for namespace/name. Convex is the only party that can
// enforce true single-use (it holds the state), so the operator always
// defers here rather than verifying anything about the token itself locally
// — see internal/gateway.Sign/Verify for what the operator mints *after*
// this call succeeds (its own session cookie, entirely local from then on).
// Returns the token's userId on success.
func (c *Client) VerifyGatewayToken(ctx context.Context, heartbeatToken, token, namespace, name string) (string, error) {
	body, err := json.Marshal(verifyGatewayTokenRequest{Token: token, Namespace: namespace, Name: name})
	if err != nil {
		return "", fmt.Errorf("marshaling verify gateway token request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL+"/operators/gateway/verify", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building verify gateway token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+heartbeatToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling verify gateway token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("verify gateway token returned status %d", resp.StatusCode)
	}

	var out verifyGatewayTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding verify gateway token response: %w", err)
	}
	return out.UserID, nil
}

type removeWorkloadRequest struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// RemoveWorkload tells Convex this workload no longer exists. Called by the
// reconciler when it observes the Workload CR is gone.
func (c *Client) RemoveWorkload(ctx context.Context, heartbeatToken, name, namespace string) error {
	body, err := json.Marshal(removeWorkloadRequest{Name: name, Namespace: namespace})
	if err != nil {
		return fmt.Errorf("marshaling remove workload request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL+"/operators/workloads/remove", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building remove workload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+heartbeatToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling remove workload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remove workload returned status %d", resp.StatusCode)
	}
	return nil
}

type metricsReportRequest struct {
	Samples []metricSampleRequest `json:"samples"`
}

type metricSampleRequest struct {
	Name      string  `json:"name"`
	Metric    string  `json:"metric"`
	Value     float64 `json:"value"`
	SampledAt int64   `json:"sampledAt"`
}

// ReportMetrics posts a batch of usage samples to Convex's
// POST /operators/metrics/report — deliberately a separate route from
// Heartbeat/ReportLifecycle (see that route's own doc comment in
// ai-cloud-v2's operators/http.ts), called on its own, longer-interval
// ticker by internal/metrics.Reporter rather than piggybacked on every
// heartbeat. Takes []metrics.Sample directly, the same way Heartbeat takes
// *capacity.Snapshot, rather than a duplicated local type. A no-op (no
// request made) for an empty batch, so a tick with nothing new to report
// never costs a round trip.
func (c *Client) ReportMetrics(ctx context.Context, heartbeatToken string, samples []metrics.Sample) error {
	if len(samples) == 0 {
		return nil
	}

	requestSamples := make([]metricSampleRequest, len(samples))
	for i, s := range samples {
		requestSamples[i] = metricSampleRequest{
			Name:      s.Name,
			Metric:    s.Metric,
			Value:     s.Value,
			SampledAt: s.SampledAt.UnixMilli(),
		}
	}

	body, err := json.Marshal(metricsReportRequest{Samples: requestSamples})
	if err != nil {
		return fmt.Errorf("marshaling metrics report request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL+"/operators/metrics/report", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building metrics report request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+heartbeatToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling metrics report: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("metrics report returned status %d", resp.StatusCode)
	}
	return nil
}
