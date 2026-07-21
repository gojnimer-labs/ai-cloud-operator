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

package metrics

import (
	"context"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// ConvexReporter is the narrow interface Reporter needs to hand off a
// collected batch — satisfied by convexclient.Runnable.ReportMetrics, the
// same narrow-interface convention already used elsewhere (e.g.
// convexclient's own capacitySnapshotter) so a test double never needs to
// fake anything about tokens/auth to substitute for this.
type ConvexReporter interface {
	ReportMetrics(ctx context.Context, samples []Sample) error
}

// SampleCollector is the narrow interface Reporter needs on the collection
// side — satisfied by *Collector. Narrowed the same way ConvexReporter is,
// so Reporter's own tests never need a real kubelet/apiserver to substitute
// for this (see Collector's own tests for that).
type SampleCollector interface {
	Collect(ctx context.Context) ([]Sample, error)
}

// Reporter periodically collects and reports usage samples on its own
// ticker — deliberately separate from convexclient.Runnable's 30s heartbeat
// loop (see ReportMetrics's own doc comment for why), so this interval is
// independently configurable and a slow/failed report never delays or
// blocks heartbeat/claim-discovery.
type Reporter struct {
	collector SampleCollector
	convex    ConvexReporter
	interval  time.Duration
}

// NewReporter returns a Reporter that collects via collector and reports
// via convex every interval.
func NewReporter(collector SampleCollector, convex ConvexReporter, interval time.Duration) *Reporter {
	return &Reporter{collector: collector, convex: convex, interval: interval}
}

// Start implements manager.Runnable.
func (r *Reporter) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.reportOnce(ctx)
		}
	}
}

// reportOnce is best-effort like every other Convex notify in this operator
// — a failed collection or report is logged and simply tried again next
// tick, no retry bookkeeping needed for what's ultimately a dashboard
// chart, not correctness-critical state.
func (r *Reporter) reportOnce(ctx context.Context) {
	log := logf.FromContext(ctx).WithName("metrics")

	samples, err := r.collector.Collect(ctx)
	if err != nil {
		log.Error(err, "collecting usage metrics")
		return
	}
	if err := r.convex.ReportMetrics(ctx, samples); err != nil {
		log.Error(err, "reporting usage metrics")
	}
}
