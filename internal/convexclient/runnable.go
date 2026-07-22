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

	"github.com/gojnimer-labs/ai-cloud-operator/internal/capacity"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/catalog"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/metrics"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/provisioning"
	"github.com/gojnimer-labs/ai-cloud-operator/internal/tokenstore"
)

// capacitySnapshotter is the narrow slice of capacity.Tracker's API this
// package depends on — matching the WorkloadNotifier/GatewayVerifier
// narrow-interface convention already used elsewhere, and letting tests
// substitute a stub without needing a real Node-backed fake client.
type capacitySnapshotter interface {
	Snapshot(ctx context.Context) (capacity.Snapshot, error)
}

// usageCollector is the narrow slice of metrics.Collector's API this package
// depends on for live-usage figures — narrowed the same way
// capacitySnapshotter narrows capacity.Tracker, so tests can substitute a
// stub without a real kubelet.
type usageCollector interface {
	CollectUsage(ctx context.Context) (metrics.UsageSnapshot, error)
}

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
	capacity          capacitySnapshotter
	usage             usageCollector

	mu     sync.RWMutex
	tokens tokenstore.Tokens
}

// RunnableConfig holds everything Runnable needs to construct — see
// NewRunnable. On every heartbeat tick, Runnable also checks Enrollment for
// an out-of-band rotation of ENROLLMENT_SECRET, re-registering immediately
// when one is found, and processes any claimable/pending-operation work the
// heartbeat returned using Creator/Destroyer (see internal/provisioning) —
// the same instances wired into internal/api.Server for the manual HTTP
// path.
type RunnableConfig struct {
	Client            *Client
	Store             *tokenstore.Store
	Enrollment        *EnrollmentSecretWatcher
	HeartbeatInterval time.Duration
	Creator           *provisioning.WorkloadCreator
	Destroyer         *provisioning.WorkloadDestroyer
	// Capacity, when set, gates processClaimable against local headroom
	// before ever calling ClaimWorkload for a candidate — see
	// internal/capacity's package doc for why this decision lives here
	// rather than on the Convex side. Nil disables the self-gate entirely
	// (every candidate is treated as fitting), matching the fail-open
	// posture tests and any future no-Convex mode rely on.
	Capacity capacitySnapshotter
	// Usage, when set, feeds heartbeatOnce's live cluster/managed usage
	// figures (see metrics.Collector.CollectUsage) — display-only, unlike
	// Capacity, never consulted by processClaimable's self-gate. Nil omits
	// the live figures from every heartbeat, same fail-open spirit as a nil
	// Capacity.
	Usage usageCollector
}

// NewRunnable builds a Runnable from cfg.
func NewRunnable(cfg RunnableConfig) *Runnable {
	return &Runnable{
		client:            cfg.Client,
		store:             cfg.Store,
		enrollment:        cfg.Enrollment,
		heartbeatInterval: cfg.HeartbeatInterval,
		creator:           cfg.Creator,
		destroyer:         cfg.Destroyer,
		capacity:          cfg.Capacity,
		usage:             cfg.Usage,
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
// presenting whatever heartbeat token is current. retryable is threaded
// straight through to the underlying claimed-workload retryable-release
// design shared with processClaimable/processPendingOperations below — most
// reconcile-loop-originated reports still pass false (the reconciler's own
// exponential-backoff requeue already handles retrying those), but
// checkUnschedulable's self-diagnosed capacity failure passes true so
// Convex releases the claim to another operator immediately instead of
// waiting out a lease.
func (r *Runnable) ReportLifecycle(ctx context.Context, name, workloadID, phase, reason string, retryable bool) error {
	r.mu.RLock()
	token := r.tokens.HeartbeatToken
	r.mu.RUnlock()
	return r.client.ReportLifecycle(ctx, token, name, workloadID, phase, reason, retryable)
}

// ReportMetrics implements internal/metrics.ConvexReporter, always
// presenting whatever heartbeat token is current — the same token-access
// pattern as every other method here, so internal/metrics.Reporter never
// needs to know about tokens/auth at all, only that it's talking to
// something that can report a batch of samples.
func (r *Runnable) ReportMetrics(ctx context.Context, samples []metrics.Sample) error {
	r.mu.RLock()
	token := r.tokens.HeartbeatToken
	r.mu.RUnlock()
	return r.client.ReportMetrics(ctx, token, samples)
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

// checkEnrollmentSecret compares the live mounted ENROLLMENT_SECRET value
// against what this operator last registered with, re-registering
// immediately on a mismatch — e.g. an operator rotating the backing Secret
// doesn't need to also restart the operator pod for it to take effect (see
// EnrollmentSecretPath).
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
// heartbeat call, and confirming this operator's own catalog, version, and
// tags haven't changed since it was issued) and falls back to fresh
// registration if none exists, the stored token is rejected, or any of
// those three fingerprints has changed — version/tags are otherwise never
// re-reported on a routine restart (an image bump, or an OPERATOR_TAGS
// edit, touches neither the catalog nor the persisted tokens' validity),
// leaving Convex's fleet table showing whatever was registered once, long
// ago.
func (r *Runnable) loadOrRegister(ctx context.Context) error {
	log := logf.FromContext(ctx)
	if tokens, ok, err := r.store.Load(ctx); err == nil && ok {
		if _, _, err := r.client.Heartbeat(ctx, tokens.HeartbeatToken, nil, nil); err == nil {
			switch {
			case tokens.CatalogHash != catalog.Hash():
				log.Info("catalog changed since last registration, re-registering")
			case tokens.OperatorVersion != r.client.Version():
				log.Info("operator version changed since last registration, re-registering")
			case tokens.TagsFingerprint != r.client.TagsFingerprint():
				log.Info("operator tags changed since last registration, re-registering")
			default:
				log.Info("reusing persisted operator token")
				r.setTokens(tokens)
				return nil
			}
		} else {
			log.Info("persisted operator token rejected by convex, re-registering")
		}
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
	tokens.CatalogHash = catalog.Hash()
	tokens.OperatorVersion = r.client.Version()
	tokens.TagsFingerprint = r.client.TagsFingerprint()
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

	// Computed once per tick and used twice: sent along with this heartbeat
	// purely for Convex's admin fleet-visibility view (see Client.Heartbeat),
	// and reused below as processClaimable's self-gate input — one
	// consistent reading for both, rather than two separately-timed ones.
	var snap *capacity.Snapshot
	if r.capacity != nil {
		s, err := r.capacity.Snapshot(ctx)
		if err != nil {
			// Fail open, same spirit as Convex's own fail-open when an
			// operator has never reported capacity: never let a snapshot
			// error block heartbeating or claiming altogether.
			logf.FromContext(ctx).Error(err, "failed to compute capacity snapshot; heartbeating and self-gating without one this tick")
		} else {
			snap = &s
		}
	}

	// live is independent of snap — a failed or unconfigured live-usage
	// collection never blocks or invalidates the (unrelated, requests-based)
	// snap above; see metrics.Collector.CollectUsage's own doc comment for
	// why a single unreachable node only undercounts rather than erroring
	// this call outright.
	var live *metrics.UsageSnapshot
	if r.usage != nil {
		u, err := r.usage.CollectUsage(ctx)
		if err != nil {
			logf.FromContext(ctx).Error(err, "failed to compute live usage snapshot; heartbeating without one this tick")
		} else {
			live = &u
		}
	}

	claimable, pendingOps, err := r.client.Heartbeat(ctx, heartbeatToken, snap, live)
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

	r.processClaimable(ctx, heartbeatToken, claimable, snap)
	r.processPendingOperations(ctx, heartbeatToken, pendingOps)
}

// processClaimable tries to claim and create every brand-new workload
// request the heartbeat surfaced (bounded by maxClaimsPerTick), enforcing
// the create-time template-version compatibility check before ever calling
// Create — see the package doc on Template.Version in internal/catalog for
// why this check exists, and the plan's "Template version compatibility"
// section for the full reasoning. Any error past the claim itself (version
// mismatch, or Create failing) is reported back to Convex immediately as a
// lifecycle event (retryable for Create, terminal for a version mismatch)
// so the row never gets stuck in "provisioning" forever.
//
// snap, when non-nil, gates each candidate against local capacity before
// ever calling ClaimWorkload — skipping (not aborting the loop on) a
// candidate that doesn't fit, since a smaller later candidate still might,
// and shrinking snap in place after each successful Create so later
// candidates in this same tick see reduced headroom (the informer cache
// backing the next tick's own Snapshot won't reflect a just-created
// Deployment until it syncs). nil (no Tracker configured, or this tick's
// Snapshot call errored) disables the gate entirely — every candidate is
// treated as fitting, the same fail-open posture Convex itself uses.
func (r *Runnable) processClaimable(ctx context.Context, heartbeatToken string, claimable []ClaimableWorkload, snap *capacity.Snapshot) {
	if r.creator == nil {
		return
	}
	log := logf.FromContext(ctx)

	for _, item := range claimable[:min(len(claimable), maxClaimsPerTick)] {
		if snap != nil {
			if tmpl, ok := catalog.Get(item.TemplateID); ok {
				estCPU, estMem := tmpl.EstimatedResources()
				if !snap.Fits(estCPU, estMem) {
					log.Info("skipping claimable workload: insufficient local capacity", "workloadId", item.WorkloadID, "templateId", item.TemplateID)
					continue
				}
			}
			// Unknown template: don't gate on it here — the post-claim
			// lookup below already handles reporting that failure.
		}

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
			// Genuinely terminal — the request itself is stale, retrying
			// against the same (now-mismatched) template can never succeed.
			if err := r.client.ReportLifecycle(ctx, heartbeatToken, "", claimed.WorkloadID, lifecyclePhaseFailed, reasonTemplateVersionMismatch, false); err != nil {
				log.Error(err, "failed to report template-version-mismatch failure", "workloadId", claimed.WorkloadID)
			}
			continue
		}

		if _, err := r.creator.Create(ctx, claimed.WorkloadID, claimed.TemplateID, claimed.UserID, claimed.Subdomain, claimed.Config); err != nil {
			log.Error(err, "failed to create claimed workload", "workloadId", claimed.WorkloadID)
			// Retryable: a transient Create failure (e.g. a momentary API
			// error) shouldn't strand this request — Convex releases it back
			// to "requested" so any tag-matching operator (including this one
			// on a future tick) can try again, up to its own attempt cap.
			if reportErr := r.client.ReportLifecycle(ctx, heartbeatToken, "", claimed.WorkloadID, lifecyclePhaseFailed, err.Error(), true); reportErr != nil {
				log.Error(reportErr, "failed to report create failure", "workloadId", claimed.WorkloadID)
			}
		} else if snap != nil {
			estCPU, estMem := tmpl.EstimatedResources()
			snap.UsedMilliCPU += estCPU
			snap.UsedMemoryBytes += estMem
		}
	}
}

// processPendingOperations tries to claim and execute every destroy/redeploy
// request already assigned to this operator that the heartbeat surfaced
// (bounded by maxPendingOperationsPerTick).
//
//   - destroy: no ReportLifecycle call on success — the existing reconciler
//     IsNotFound branch (notifyRemoved, generalized Convex-side to fire from
//     any status) stays the sole trigger for the "destroyed" transition,
//     covering both this claimed path and an out-of-band kubectl delete
//     identically. A synchronous Destroy error IS now reported (retryable),
//     closing what used to be a silent gap: previously nothing told Convex
//     about the failure, and it only got retried at all because Convex kept
//     re-surfacing the same pending destroy operation on future heartbeats —
//     an accidental, uncapped retry with no visibility. Now it goes through
//     the same releaseClaim/attempt-cap machinery as every other in-flight
//     status, and eventually terminal-fails with a clear reason instead of
//     retrying forever.
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
		log.Info("claimed pending operation", "operation", op.Operation, "name", op.Name, "workloadId", item.WorkloadID)

		switch op.Operation {
		case operationDestroy:
			if r.destroyer == nil {
				continue
			}
			if err := r.destroyer.Destroy(ctx, op.Name); err != nil {
				log.Error(err, "failed to destroy claimed workload", "name", op.Name)
				// Retryable: destroying has no non-retryable resolution on
				// the Convex side (see reportLifecycleRequest.Retryable) —
				// this MUST always be true here.
				if reportErr := r.client.ReportLifecycle(ctx, heartbeatToken, op.Name, "", lifecyclePhaseFailed, err.Error(), true); reportErr != nil {
					log.Error(reportErr, "failed to report destroy failure", "name", op.Name)
				}
			} else {
				log.Info("destroyed claimed workload", "name", op.Name)
			}
		case operationRedeploy:
			if r.creator == nil {
				continue
			}
			tmpl, ok := catalog.Get(op.TemplateID)
			if !ok || tmpl.Version != op.TemplateVersion {
				log.Info("redeploy claim failed template-version check", "name", op.Name, "templateId", op.TemplateID, "claimedVersion", op.TemplateVersion, "catalogFound", ok, "catalogVersion", tmpl.Version)
				// Genuinely terminal, same reasoning as create's own check.
				if err := r.client.ReportLifecycle(ctx, heartbeatToken, op.Name, "", lifecyclePhaseFailed, reasonTemplateVersionMismatch, false); err != nil {
					log.Error(err, "failed to report template-version-mismatch failure", "name", op.Name)
				}
				continue
			}
			if err := r.creator.Redeploy(ctx, op.Name, op.Config); err != nil {
				log.Error(err, "failed to redeploy claimed workload", "name", op.Name)
				if reportErr := r.client.ReportLifecycle(ctx, heartbeatToken, op.Name, "", lifecyclePhaseFailed, err.Error(), true); reportErr != nil {
					log.Error(reportErr, "failed to report redeploy failure", "name", op.Name)
				}
			} else {
				log.Info("redeployed claimed workload, waiting on reconciler to report active", "name", op.Name)
			}
		case operationStop, operationResume:
			if r.creator == nil {
				continue
			}
			if err := r.creator.SetSuspended(ctx, op.Name, op.Operation == operationStop); err != nil {
				log.Error(err, "failed to set suspended state for claimed workload", "name", op.Name, "operation", op.Operation)
				if reportErr := r.client.ReportLifecycle(ctx, heartbeatToken, op.Name, "", lifecyclePhaseFailed, err.Error(), true); reportErr != nil {
					log.Error(reportErr, "failed to report stop/resume failure", "name", op.Name)
				}
			} else {
				log.Info("set suspended state for claimed workload, waiting on reconciler to report outcome", "name", op.Name, "operation", op.Operation)
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
