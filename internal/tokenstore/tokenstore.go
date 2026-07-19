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

// Package tokenstore persists the bearer tokens issued by Convex during
// operator registration into a Kubernetes Secret, so the operator does not
// need to re-register on every restart.
package tokenstore

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gojnimer-labs/ai-cloud-operator/internal/labels"
)

// SecretName is the name of the Secret the operator uses to persist its
// issued tokens across restarts.
const SecretName = "ai-cloud-operator-token"

const (
	keyHeartbeatToken = "heartbeatToken"
	keyDeployToken    = "deployToken"
	keyCatalogHash    = "catalogHash"
)

// Tokens holds the pair of bearer tokens minted at registration time, plus
// the catalog fingerprint (see internal/catalog.Hash) this operator last
// successfully registered with Convex.
//
// HeartbeatToken is presented BY the operator when calling Convex's
// heartbeat endpoint; Convex only ever stores its hash.
// DeployToken is presented BY Convex when calling the operator's inbound
// HTTP API; the operator only ever stores its hash (see internal/api).
// CatalogHash lets Runnable.loadOrRegister detect, at startup, that the
// compiled-in catalog differs from what was last reported — the persisted
// token Secret otherwise survives a restart and would silently reuse a
// stale registration.
type Tokens struct {
	HeartbeatToken string
	DeployToken    string
	CatalogHash    string
}

// create cannot be scoped by resourceNames in Kubernetes RBAC (the object
// doesn't exist yet), but get/update are restricted to the one Secret this
// package manages — deliberately narrower than v1's external cluster-admin-ish
// static token.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;update,resourceNames=ai-cloud-operator-token

// Store reads and writes the operator's issued Tokens from a Kubernetes
// Secret in a single namespace.
type Store struct {
	client    client.Client
	namespace string
}

// New returns a Store that persists tokens into a Secret named SecretName in
// namespace.
func New(c client.Client, namespace string) *Store {
	return &Store{client: c, namespace: namespace}
}

// Load returns the currently persisted Tokens, or (Tokens{}, false, nil) if
// no Secret has been written yet.
func (s *Store) Load(ctx context.Context) (Tokens, bool, error) {
	var secret corev1.Secret
	err := s.client.Get(ctx, client.ObjectKey{Name: SecretName, Namespace: s.namespace}, &secret)
	if apierrors.IsNotFound(err) {
		return Tokens{}, false, nil
	}
	if err != nil {
		return Tokens{}, false, fmt.Errorf("getting token secret: %w", err)
	}

	tokens := Tokens{
		HeartbeatToken: string(secret.Data[keyHeartbeatToken]),
		DeployToken:    string(secret.Data[keyDeployToken]),
		CatalogHash:    string(secret.Data[keyCatalogHash]),
	}
	if tokens.HeartbeatToken == "" || tokens.DeployToken == "" {
		return Tokens{}, false, nil
	}
	return tokens, true, nil
}

// Save creates or updates the Secret holding tokens.
func (s *Store) Save(ctx context.Context, tokens Tokens) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName,
			Namespace: s.namespace,
		},
	}

	err := s.client.Get(ctx, client.ObjectKey{Name: SecretName, Namespace: s.namespace}, secret)
	switch {
	case apierrors.IsNotFound(err):
		secret.Labels = map[string]string{labels.ManagedBy: labels.ManagedByValue}
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{
			keyHeartbeatToken: []byte(tokens.HeartbeatToken),
			keyDeployToken:    []byte(tokens.DeployToken),
			keyCatalogHash:    []byte(tokens.CatalogHash),
		}
		if err := s.client.Create(ctx, secret); err != nil {
			return fmt.Errorf("creating token secret: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("getting token secret before update: %w", err)
	default:
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[keyHeartbeatToken] = []byte(tokens.HeartbeatToken)
		secret.Data[keyDeployToken] = []byte(tokens.DeployToken)
		secret.Data[keyCatalogHash] = []byte(tokens.CatalogHash)
		if err := s.client.Update(ctx, secret); err != nil {
			return fmt.Errorf("updating token secret: %w", err)
		}
		return nil
	}
}
