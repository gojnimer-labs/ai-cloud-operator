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
	"context"
	"crypto/rand"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KeySecretName is the Secret the operator uses to persist the key it
// self-generates for signing gateway session cookies (see Sign/Verify).
// Unlike ENROLLMENT_SECRET, nothing outside this operator instance ever
// needs to know this value, so there is no reason to make an operator ask a
// human to mint and hand it one.
const KeySecretName = "ai-cloud-operator-gateway-key"

const (
	keySigningKey = "signingKey"

	keyLabelManagedBy = "app.kubernetes.io/managed-by"
	keyManagedByValue = "ai-cloud-operator"

	signingKeyBytes = 32
)

// create cannot be scoped by resourceNames (the object doesn't exist yet);
// get/update are restricted to the one Secret this store manages.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;update,resourceNames=ai-cloud-operator-gateway-key

// KeyStore reads and, if necessary, generates and persists this operator's
// gateway cookie-signing key in a Kubernetes Secret in a single namespace.
type KeyStore struct {
	client    client.Client
	namespace string
}

// NewKeyStore returns a KeyStore that persists the signing key into a Secret
// named KeySecretName in namespace.
func NewKeyStore(c client.Client, namespace string) *KeyStore {
	return &KeyStore{client: c, namespace: namespace}
}

// LoadOrGenerate returns the persisted signing key, generating a fresh
// random one and persisting it on first use. Concurrent callers racing to
// create the Secret (e.g. two replicas starting at once) converge on
// whichever one wins the create — the loser re-reads and uses that value
// rather than its own, so every replica ends up signing with the same key.
func (s *KeyStore) LoadOrGenerate(ctx context.Context) ([]byte, error) {
	var secret corev1.Secret
	err := s.client.Get(ctx, client.ObjectKey{Name: KeySecretName, Namespace: s.namespace}, &secret)
	if err == nil {
		if key := secret.Data[keySigningKey]; len(key) > 0 {
			return key, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("getting gateway key secret: %w", err)
	}

	key := make([]byte, signingKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating gateway signing key: %w", err)
	}

	created := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      KeySecretName,
			Namespace: s.namespace,
			Labels:    map[string]string{keyLabelManagedBy: keyManagedByValue},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{keySigningKey: key},
	}
	if err := s.client.Create(ctx, created); err != nil {
		if apierrors.IsAlreadyExists(err) {
			var existing corev1.Secret
			if getErr := s.client.Get(ctx, client.ObjectKey{Name: KeySecretName, Namespace: s.namespace}, &existing); getErr != nil {
				return nil, fmt.Errorf("getting gateway key secret after lost create race: %w", getErr)
			}
			if existingKey := existing.Data[keySigningKey]; len(existingKey) > 0 {
				return existingKey, nil
			}
			return nil, fmt.Errorf("gateway key secret exists but has no %s data", keySigningKey)
		}
		return nil, fmt.Errorf("creating gateway key secret: %w", err)
	}
	return key, nil
}
