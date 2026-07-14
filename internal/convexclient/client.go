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

	"github.com/gojnimer-labs/ai-cloud-operator/internal/tokenstore"
)

// ErrUnauthorized indicates Convex rejected the presented credential (401 or
// 410) — the caller should treat this as "re-register".
var ErrUnauthorized = fmt.Errorf("convex rejected credential")

// Config holds the operator's identity and how to reach Convex.
type Config struct {
	BaseURL          string
	EnrollmentSecret string
	OperatorName     string
	ExternalURL      string
	Metadata         map[string]any
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

type registerRequest struct {
	Name             string         `json:"name"`
	ExternalURL      string         `json:"externalUrl"`
	EnrollmentSecret string         `json:"enrollmentSecret"`
	Metadata         map[string]any `json:"metadata,omitempty"`
}

type registerResponse struct {
	HeartbeatToken string `json:"heartbeatToken"`
	DeployToken    string `json:"deployToken"`
}

// Register performs one-time enrollment with Convex using the pre-shared
// enrollment secret, returning the pair of tokens Convex minted for this
// operator.
func (c *Client) Register(ctx context.Context) (tokenstore.Tokens, error) {
	body, err := json.Marshal(registerRequest{
		Name:             c.config.OperatorName,
		ExternalURL:      c.config.ExternalURL,
		EnrollmentSecret: c.config.EnrollmentSecret,
		Metadata:         c.config.Metadata,
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
}

// Heartbeat reports liveness to Convex using the previously issued heartbeat
// token. Returns ErrUnauthorized if Convex rejects the token (401/410),
// signaling the caller should discard it and re-register.
func (c *Client) Heartbeat(ctx context.Context, heartbeatToken string) error {
	body, err := json.Marshal(heartbeatRequest{Name: c.config.OperatorName})
	if err != nil {
		return fmt.Errorf("marshaling heartbeat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL+"/operators/heartbeat", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building heartbeat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+heartbeatToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling heartbeat: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusGone:
		return ErrUnauthorized
	default:
		return fmt.Errorf("heartbeat returned status %d", resp.StatusCode)
	}
}

// WorkloadInfo is the ownership/identity data the reconciler reports back to
// Convex — deliberately just the fields already on Workload.Spec, never
// runtime status/phase (that stays fetched live, never mirrored).
type WorkloadInfo struct {
	Name         string
	Namespace    string
	TemplateName string
	UserID       string
	Subdomain    string
}

type upsertWorkloadRequest struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	TemplateID string `json:"templateId"`
	UserID     string `json:"userId"`
	Subdomain  string `json:"subdomain,omitempty"`
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
