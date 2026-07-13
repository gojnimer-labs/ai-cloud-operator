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

package convexclient

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gojnimer-labs/ai-cloud-operator/internal/tokenstore"
)

// Runnable registers this operator with Convex on startup (reusing a
// previously persisted token when possible) and heartbeats on a fixed
// interval thereafter. It implements controller-runtime's manager.Runnable
// so the manager starts/stops it alongside the reconcile loop.
type Runnable struct {
	client            *Client
	store             *tokenstore.Store
	heartbeatInterval time.Duration

	mu     sync.RWMutex
	tokens tokenstore.Tokens
}

// NewRunnable builds a Runnable that talks to Convex via client and persists
// tokens via store, heartbeating every heartbeatInterval.
func NewRunnable(client *Client, store *tokenstore.Store, heartbeatInterval time.Duration) *Runnable {
	return &Runnable{
		client:            client,
		store:             store,
		heartbeatInterval: heartbeatInterval,
	}
}

// CurrentDeployToken returns the deploy token Convex should currently be
// presenting when it calls this operator's inbound HTTP API. Empty until
// registration completes.
func (r *Runnable) CurrentDeployToken() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tokens.DeployToken
}

// UpsertWorkload and RemoveWorkload implement
// internal/controller.WorkloadNotifier, always presenting whatever
// heartbeat token is current (it may have rotated since the reconciler's
// last call).
func (r *Runnable) UpsertWorkload(ctx context.Context, info WorkloadInfo) error {
	r.mu.RLock()
	token := r.tokens.HeartbeatToken
	r.mu.RUnlock()
	return r.client.UpsertWorkload(ctx, token, info)
}

func (r *Runnable) RemoveWorkload(ctx context.Context, name, namespace string) error {
	r.mu.RLock()
	token := r.tokens.HeartbeatToken
	r.mu.RUnlock()
	return r.client.RemoveWorkload(ctx, token, name, namespace)
}

// Start implements manager.Runnable. It blocks until ctx is cancelled.
func (r *Runnable) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("convexclient")

	if err := r.loadOrRegister(ctx, log); err != nil {
		return err
	}

	ticker := time.NewTicker(r.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.heartbeatOnce(ctx, log)
		}
	}
}

// loadOrRegister tries to reuse a persisted token (validating it with one
// heartbeat call) and falls back to fresh registration if none exists or the
// stored token is rejected.
func (r *Runnable) loadOrRegister(ctx context.Context, log logr.Logger) error {
	if tokens, ok, err := r.store.Load(ctx); err == nil && ok {
		if err := r.client.Heartbeat(ctx, tokens.HeartbeatToken); err == nil {
			log.Info("reusing persisted operator token")
			r.setTokens(tokens)
			return nil
		}
		log.Info("persisted operator token rejected by convex, re-registering")
	} else if err != nil {
		log.Error(err, "failed to load persisted token, will register fresh")
	}

	return r.register(ctx, log)
}

func (r *Runnable) register(ctx context.Context, log logr.Logger) error {
	tokens, err := r.client.Register(ctx)
	if err != nil {
		return err
	}
	if err := r.store.Save(ctx, tokens); err != nil {
		return err
	}
	log.Info("registered with convex")
	r.setTokens(tokens)
	return nil
}

func (r *Runnable) heartbeatOnce(ctx context.Context, log logr.Logger) {
	r.mu.RLock()
	heartbeatToken := r.tokens.HeartbeatToken
	r.mu.RUnlock()

	err := r.client.Heartbeat(ctx, heartbeatToken)
	if err == nil {
		return
	}

	if err != ErrUnauthorized {
		log.Error(err, "heartbeat failed")
		return
	}

	log.Info("heartbeat token rejected by convex, re-registering")
	if err := r.register(ctx, log); err != nil {
		log.Error(err, "re-registration failed")
	}
}

func (r *Runnable) setTokens(tokens tokenstore.Tokens) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens = tokens
}
