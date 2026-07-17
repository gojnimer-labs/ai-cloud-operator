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

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gojnimer-labs/ai-cloud-operator/internal/catalog"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/provisioning"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/tokenstore"
)

const (
	// maxClaimsPerTick/maxPendingOperationsPerTick bound how much claimable
	// work a single heartbeat tick picks up — matching Convex's own
	// listClaimable/listPendingOperations .take(20), just smaller, so one
	// slow Create/Redeploy/Destroy can't stall an entire tick's worth of
	// otherwise-independent work indefinitely. Unclaimed remainder is picked
	// up on the next tick.
	maxClaimsPerTick            = 5
	maxPendingOperationsPerTick = 5

	operationDestroy  = "destroy"
	operationRedeploy = "redeploy"
	operationStop     = "stop"
	operationResume   = "resume"

	lifecyclePhaseFailed = "failed"

	reasonTemplateVersionMismatch = "template changed since request; please resubmit"
)

// Runnable registers this operator with Convex on startup (reusing a
// previously persisted token when possible) and heartbeats on a fixed
// interval thereafter. Every successful heartbeat also returns claimable
// work (see Client.Heartbeat) that this Runnable immediately tries to claim
// and act on — see processClaimable/processPendingOperations. It implements
// controller-runtime's manager.Runnable so the manager starts/stops it
// alongside the reconcile loop.
type Runnable struct {
	client            *Client
	store             *tokenstore.Store
	enrollment        *EnrollmentSecretWatcher
	heartbeatInterval time.Duration
	creator           *provisioning.WorkloadCreator
	destroyer         *provisioning.WorkloadDestroyer

	mu     sync.RWMutex
	tokens tokenstore.Tokens
}

// NewRunnable builds a Runnable that talks to Convex via client and persists
// tokens via store, heartbeating every heartbeatInterval. On every heartbeat
// tick it also checks enrollment for an out-of-band rotation of
// ENROLLMENT_SECRET, re-registering immediately when one is found, and
// processes any claimable/pending-operation work the heartbeat returned
// using creator/destroyer (see internal/provisioning) — the same instances
// wired into internal/api.Server for the manual HTTP path.
func NewRunnable(client *Client, store *tokenstore.Store, enrollment *EnrollmentSecretWatcher, heartbeatInterval time.Duration, creator *provisioning.WorkloadCreator, destroyer *provisioning.WorkloadDestroyer) *Runnable {
	return &Runnable{
		client:            client,
		store:             store,
		enrollment:        enrollment,
		heartbeatInterval: heartbeatInterval,
		creator:           creator,
		destroyer:         destroyer,
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

// ReportLifecycle implements internal/controller.WorkloadNotifier, always
// presenting whatever heartbeat token is current.
func (r *Runnable) ReportLifecycle(ctx context.Context, name, workloadID, phase, reason string) error {
	r.mu.RLock()
	token := r.tokens.HeartbeatToken
	r.mu.RUnlock()
	return r.client.ReportLifecycle(ctx, token, name, workloadID, phase, reason)
}

// VerifyGatewayToken implements internal/api.GatewayVerifier, always
// presenting whatever heartbeat token is current.
func (r *Runnable) VerifyGatewayToken(ctx context.Context, token, namespace, name string) (string, error) {
	r.mu.RLock()
	heartbeatToken := r.tokens.HeartbeatToken
	r.mu.RUnlock()
	return r.client.VerifyGatewayToken(ctx, heartbeatToken, token, namespace, name)
}

// Start implements manager.Runnable. It blocks until ctx is cancelled.
func (r *Runnable) Start(ctx context.Context) error {
	ctx = logf.IntoContext(ctx, logf.FromContext(ctx).WithName("convexclient"))

	if err := r.loadOrRegister(ctx); err != nil {
		return err
	}

	ticker := time.NewTicker(r.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.heartbeatOnce(ctx)
			r.checkEnrollmentSecret(ctx)
		}
	}
}

// checkEnrollmentSecret compares the live ENROLLMENT_SECRET Secret against
// what this operator last registered with, re-registering immediately on a
// mismatch — e.g. an operator re-running the kubectl create secret step to
// rotate the value doesn't need to also restart the operator pod for it to
// take effect.
func (r *Runnable) checkEnrollmentSecret(ctx context.Context) {
	log := logf.FromContext(ctx)

	current, err := r.enrollment.Current(ctx)
	if err != nil {
		log.Error(err, "failed to read enrollment secret")
		return
	}
	if current == "" || current == r.client.EnrollmentSecret() {
		return
	}

	log.Info("enrollment secret changed, re-registering")
	r.client.SetEnrollmentSecret(current)
	if err := r.register(ctx); err != nil {
		log.Error(err, "re-registration after enrollment secret change failed")
	}
}

// loadOrRegister tries to reuse a persisted token (validating it with one
// heartbeat call) and falls back to fresh registration if none exists or the
// stored token is rejected.
func (r *Runnable) loadOrRegister(ctx context.Context) error {
	log := logf.FromContext(ctx)
	if tokens, ok, err := r.store.Load(ctx); err == nil && ok {
		if _, _, err := r.client.Heartbeat(ctx, tokens.HeartbeatToken); err == nil {
			log.Info("reusing persisted operator token")
			r.setTokens(tokens)
			return nil
		}
		log.Info("persisted operator token rejected by convex, re-registering")
	} else if err != nil {
		log.Error(err, "failed to load persisted token, will register fresh")
	}

	return r.register(ctx)
}

func (r *Runnable) register(ctx context.Context) error {
	tokens, err := r.client.Register(ctx)
	if err != nil {
		return err
	}
	if err := r.store.Save(ctx, tokens); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("registered with convex")
	r.setTokens(tokens)
	return nil
}

func (r *Runnable) heartbeatOnce(ctx context.Context) {
	r.mu.RLock()
	heartbeatToken := r.tokens.HeartbeatToken
	r.mu.RUnlock()

	claimable, pendingOps, err := r.client.Heartbeat(ctx, heartbeatToken)
	if err != nil {
		log := logf.FromContext(ctx)
		if err != ErrUnauthorized {
			log.Error(err, "heartbeat failed")
			return
		}

		log.Info("heartbeat token rejected by convex, re-registering")
		if err := r.register(ctx); err != nil {
			log.Error(err, "re-registration failed")
		}
		return
	}

	r.processClaimable(ctx, heartbeatToken, claimable)
	r.processPendingOperations(ctx, heartbeatToken, pendingOps)
}

// processClaimable tries to claim and create every brand-new workload
// request the heartbeat surfaced (bounded by maxClaimsPerTick), enforcing
// the create-time template-version compatibility check before ever calling
// Create — see the package doc on Template.Version in internal/catalog for
// why this check exists, and the plan's "Template version compatibility"
// section for the full reasoning. Any error past the claim itself (version
// mismatch, or Create failing) is reported back to Convex immediately as a
// "failed" lifecycle event so the row never gets stuck in "provisioning"
// forever — there's no drift-detector (out of scope) that would otherwise
// ever notice.
func (r *Runnable) processClaimable(ctx context.Context, heartbeatToken string, claimable []ClaimableWorkload) {
	if r.creator == nil {
		return
	}
	log := logf.FromContext(ctx)

	for _, item := range claimable[:min(len(claimable), maxClaimsPerTick)] {
		claimed, err := r.client.ClaimWorkload(ctx, heartbeatToken, item.WorkloadID)
		if err != nil {
			log.Error(err, "failed to claim workload", "workloadId", item.WorkloadID)
			continue
		}
		if claimed == nil {
			// Lost the race to another operator, or it's no longer
			// claimable — nothing to do.
			continue
		}

		tmpl, ok := catalog.Get(claimed.TemplateID)
		if !ok || tmpl.Version != claimed.TemplateVersion {
			if err := r.client.ReportLifecycle(ctx, heartbeatToken, "", claimed.WorkloadID, lifecyclePhaseFailed, reasonTemplateVersionMismatch); err != nil {
				log.Error(err, "failed to report template-version-mismatch failure", "workloadId", claimed.WorkloadID)
			}
			continue
		}

		if _, err := r.creator.Create(ctx, claimed.WorkloadID, claimed.TemplateID, claimed.UserID, claimed.Subdomain, claimed.Config); err != nil {
			log.Error(err, "failed to create claimed workload", "workloadId", claimed.WorkloadID)
			if reportErr := r.client.ReportLifecycle(ctx, heartbeatToken, "", claimed.WorkloadID, lifecyclePhaseFailed, err.Error()); reportErr != nil {
				log.Error(reportErr, "failed to report create failure", "workloadId", claimed.WorkloadID)
			}
		}
	}
}

// processPendingOperations tries to claim and execute every destroy/redeploy
// request already assigned to this operator that the heartbeat surfaced
// (bounded by maxPendingOperationsPerTick).
//
//   - destroy: no immediate ReportLifecycle call on either success or
//     failure — destroy has no "failed" status in the unified status model
//     (see the plan), so the existing reconciler IsNotFound branch
//     (notifyRemoved, generalized Convex-side to fire from any status) stays
//     the sole trigger for the "destroyed" transition, covering both this
//     claimed path and an out-of-band kubectl delete identically. A
//     synchronous Destroy error just gets retried on a future heartbeat tick
//     (Convex still shows the row as "destroying" until it actually
//     succeeds).
//   - redeploy: same template-version compatibility check as create, same
//     immediate "failed" report on mismatch or a synchronous Redeploy error.
//     On success, status intentionally stays "redeploying" until the
//     reconciler's normal Running-transition fires the generalized
//     active-report call site (see workload_controller.go) — the exact same
//     mechanism a fresh create's first Running transition uses, no separate
//     signal needed.
//   - stop/resume: WorkloadCreator.SetSuspended(ctx, name, operation ==
//     "stop") flips Spec.Suspended and lets the reconciler's
//     desiredReplicaCount/phaseStopped machinery do the rest. Same immediate
//     "failed" report on a synchronous SetSuspended error as
//     destroy/redeploy; on success, status stays "stopping"/"resuming" until
//     the reconciler's normal Stopped/Running-transition fires the
//     generalized syncConvexLifecyclePhase call site, same mechanism as
//     redeploy's "active" report.
func (r *Runnable) processPendingOperations(ctx context.Context, heartbeatToken string, pendingOps []PendingOperation) {
	if r.creator == nil && r.destroyer == nil {
		return
	}
	log := logf.FromContext(ctx)

	for _, item := range pendingOps[:min(len(pendingOps), maxPendingOperationsPerTick)] {
		op, err := r.client.ClaimOperation(ctx, heartbeatToken, item.WorkloadID)
		if err != nil {
			log.Error(err, "failed to claim operation", "workloadId", item.WorkloadID)
			continue
		}
		if op == nil {
			// Lost the race, or it's no longer claimable — nothing to do.
			continue
		}

		switch op.Operation {
		case operationDestroy:
			if r.destroyer == nil {
				continue
			}
			if err := r.destroyer.Destroy(ctx, op.Name); err != nil {
				log.Error(err, "failed to destroy claimed workload", "name", op.Name)
			}
		case operationRedeploy:
			if r.creator == nil {
				continue
			}
			tmpl, ok := catalog.Get(op.TemplateID)
			if !ok || tmpl.Version != op.TemplateVersion {
				if err := r.client.ReportLifecycle(ctx, heartbeatToken, op.Name, "", lifecyclePhaseFailed, reasonTemplateVersionMismatch); err != nil {
					log.Error(err, "failed to report template-version-mismatch failure", "name", op.Name)
				}
				continue
			}
			if err := r.creator.Redeploy(ctx, op.Name, op.Config); err != nil {
				log.Error(err, "failed to redeploy claimed workload", "name", op.Name)
				if reportErr := r.client.ReportLifecycle(ctx, heartbeatToken, op.Name, "", lifecyclePhaseFailed, err.Error()); reportErr != nil {
					log.Error(reportErr, "failed to report redeploy failure", "name", op.Name)
				}
			}
		case operationStop, operationResume:
			if r.creator == nil {
				continue
			}
			if err := r.creator.SetSuspended(ctx, op.Name, op.Operation == operationStop); err != nil {
				log.Error(err, "failed to set suspended state for claimed workload", "name", op.Name, "operation", op.Operation)
				if reportErr := r.client.ReportLifecycle(ctx, heartbeatToken, op.Name, "", lifecyclePhaseFailed, err.Error()); reportErr != nil {
					log.Error(reportErr, "failed to report stop/resume failure", "name", op.Name)
				}
			}
		default:
			log.Info("ignoring pending operation of unknown kind", "operation", op.Operation, "name", op.Name)
		}
	}
}

func (r *Runnable) setTokens(tokens tokenstore.Tokens) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens = tokens
}
