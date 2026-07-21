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
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeCollector struct {
	samples []Sample
	err     error
	calls   atomic.Int32
}

func (f *fakeCollector) Collect(_ context.Context) ([]Sample, error) {
	f.calls.Add(1)
	return f.samples, f.err
}

type fakeConvexReporter struct {
	err       error
	reported  atomic.Int32
	lastCount int
}

func (f *fakeConvexReporter) ReportMetrics(_ context.Context, samples []Sample) error {
	f.reported.Add(1)
	f.lastCount = len(samples)
	return f.err
}

func TestReporterCollectsAndReportsOnEachTick(t *testing.T) {
	collector := &fakeCollector{samples: []Sample{{Name: "wl-1", Metric: MetricNetworkRxBytes, Value: 100, SampledAt: time.Now()}}}
	convex := &fakeConvexReporter{}
	r := NewReporter(collector, convex, time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_ = r.Start(ctx)

	if collector.calls.Load() == 0 {
		t.Fatalf("expected at least one Collect call")
	}
	if convex.reported.Load() == 0 {
		t.Fatalf("expected at least one ReportMetrics call")
	}
	if convex.lastCount != 1 {
		t.Fatalf("expected the collected sample to be passed through, got %d samples", convex.lastCount)
	}
}

func TestReporterStopsOnContextCancellation(t *testing.T) {
	collector := &fakeCollector{}
	convex := &fakeConvexReporter{}
	r := NewReporter(collector, convex, time.Hour) // long enough that only cancellation ends Start

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected Start to return nil on cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not return promptly after context cancellation")
	}
}

func TestReporterSkipsReportOnCollectError(t *testing.T) {
	collector := &fakeCollector{err: errors.New("boom")}
	convex := &fakeConvexReporter{}
	r := NewReporter(collector, convex, time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_ = r.Start(ctx)

	if convex.reported.Load() != 0 {
		t.Fatalf("expected ReportMetrics never called when Collect fails, got %d calls", convex.reported.Load())
	}
}
