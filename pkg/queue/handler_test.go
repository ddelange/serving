/*
Copyright 2020 The Knative Authors

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

package queue

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	netheader "knative.dev/networking/pkg/http/header"
	netstats "knative.dev/networking/pkg/http/stats"
	"knative.dev/serving/pkg/activator"
)

const (
	wantHost = "a-better-host.com"
)

func TestHandlerBreakerQueueFull(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	tracer := tp.Tracer("test")

	// This test sends three requests of which one should fail immediately as the queue
	// saturates.
	resp := make(chan struct{})
	blockHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-resp
	})
	breaker := NewBreaker(BreakerParams{
		QueueDepth: 1, MaxConcurrency: 1, InitialCapacity: 1,
	})
	stats := netstats.NewRequestStats(time.Now())
	h := ProxyHandler(tracer, breaker, stats, blockHandler)

	req := httptest.NewRequest(http.MethodGet, "http://localhost:8081/time", nil)
	resps := make(chan *httptest.ResponseRecorder)
	for range 3 {
		go func() {
			rec := httptest.NewRecorder()
			h(rec, req)
			resps <- rec
		}()
	}

	// One of the three requests fails and it should be the first we see since the others
	// are still held by the resp channel.
	failure := <-resps
	if got, want := failure.Code, http.StatusServiceUnavailable; got != want {
		t.Errorf("Code = %d, want: %d", got, want)
	}
	const want = "pending request queue full"
	if got := failure.Body.String(); !strings.Contains(failure.Body.String(), want) {
		t.Errorf("Body = %q wanted to contain %q", got, want)
	}

	// Allow the remaining requests to pass.
	close(resp)
	for range 2 {
		res := <-resps
		if got, want := res.Code, http.StatusOK; got != want {
			t.Errorf("Code = %d, want: %d", got, want)
		}
	}
}

func TestHandlerBreakerTimeout(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	tracer := tp.Tracer("test")

	// This test sends a request which will take a long time to complete.
	// Then another one with a very short context timeout.
	// Verifies that the second one fails with timeout.
	seen := make(chan struct{})
	resp := make(chan struct{})
	defer close(resp) // Allow all requests to pass through.
	blockHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- struct{}{}
		<-resp
	})
	breaker := NewBreaker(BreakerParams{
		QueueDepth: 1, MaxConcurrency: 1, InitialCapacity: 1,
	})
	stats := netstats.NewRequestStats(time.Now())
	h := ProxyHandler(tracer, breaker, stats, blockHandler)

	go func() {
		h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://localhost:8081/time", nil))
	}()

	// Wait until the first request has entered the handler.
	<-seen

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "http://localhost:8081/time", nil).WithContext(ctx))
	if got, want := rec.Code, http.StatusServiceUnavailable; got != want {
		t.Fatalf("Code = %d, want: %d", got, want)
	}

	want := context.DeadlineExceeded.Error()
	if got := rec.Body.String(); !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("Body = %q wanted to contain %q", got, want)
	}
}

func TestHandlerReqEvent(t *testing.T) {
	params := BreakerParams{QueueDepth: 10, MaxConcurrency: 10, InitialCapacity: 10}
	breaker := NewBreaker(params)
	for _, br := range []*Breaker{breaker, nil} {
		t.Run(fmt.Sprint("Breaker?=", br == nil), func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
			tracer := tp.Tracer("test")

			// This has to be here to capture subtest.
			var httpHandler http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get(activator.RevisionHeaderName) != "" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}

				if r.Header.Get(activator.RevisionHeaderNamespace) != "" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}

				if got, want := r.Host, wantHost; got != want {
					t.Errorf("Host header = %q, want: %q", got, want)
				}
				if got, want := r.Header.Get(netheader.OriginalHostKey), ""; got != want {
					t.Errorf("%s header was preserved", netheader.OriginalHostKey)
				}

				w.WriteHeader(http.StatusOK)
			}

			server := httptest.NewServer(httpHandler)
			serverURL, _ := url.Parse(server.URL)

			defer server.Close()
			proxy := httputil.NewSingleHostReverseProxy(serverURL)

			stats := netstats.NewRequestStats(time.Now())
			h := ProxyHandler(tracer, br, stats, proxy)

			writer := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "http://example.com", nil)

			// Verify the Original host header processing.
			req.Host = "nimporte.pas"
			req.Header.Set(netheader.OriginalHostKey, wantHost)
			req.Header.Set(netheader.ProxyKey, activator.Name)
			h(writer, req)

			if got := stats.Report(time.Now()).ProxiedRequestCount; got != 1 {
				t.Errorf("ProxiedRequestCount = %v, want 1", got)
			}
		})
	}
}

func TestIgnoreProbe(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
	tracer := tp.Tracer("test")

	// Verifies that probes don't queue.
	resp := make(chan struct{})
	var c atomic.Int32
	// Ensure we can receive 3 requests with CC=1.
	go func() {
		to := time.After(3 * time.Second)
		tick := time.NewTicker(10 * time.Millisecond)
		defer func() { tick.Stop() }()
		for {
			select {
			case <-tick.C:
				if c.Load() == 3 {
					close(resp)
					return
				}
			case <-to:
				// No fatal'ing in goroutines.
				t.Error("Timed out waiting to see 3 probes")
				return
			}
		}
	}()

	var httpHandler http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
		c.Add(1)
		<-resp
		if !netheader.IsKubeletProbe(r) {
			t.Error("Request was not a probe")
			w.WriteHeader(http.StatusBadRequest)
		}
	}

	server := httptest.NewServer(httpHandler)
	serverURL, _ := url.Parse(server.URL)

	defer server.Close()
	proxy := httputil.NewSingleHostReverseProxy(serverURL)

	// Ensure no more than 1 request can be queued. So we'll send 3.
	breaker := NewBreaker(BreakerParams{QueueDepth: 1, MaxConcurrency: 1, InitialCapacity: 1})
	stats := netstats.NewRequestStats(time.Now())
	h := ProxyHandler(tracer, breaker, stats, proxy)

	req := httptest.NewRequest(http.MethodPost, "http://prob.in", nil)
	req.Header.Set("User-Agent", netheader.KubeProbeUAPrefix+"1.29") // Mark it a probe.
	go h(httptest.NewRecorder(), req)
	go h(httptest.NewRecorder(), req)

	// Last one got synchronously.
	w := httptest.NewRecorder()
	h(w, req)

	if got, want := w.Code, http.StatusOK; got != want {
		t.Errorf("Status got = %d, want: %d", got, want)
	}
}

func BenchmarkProxyHandler(b *testing.B) {
	baseHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	stats := netstats.NewRequestStats(time.Now())

	req := httptest.NewRequest(http.MethodPost, "http://example.com", nil)
	req.Header.Set(netheader.OriginalHostKey, wantHost)

	tests := []struct {
		label        string
		breaker      *Breaker
		reportPeriod time.Duration
	}{{
		label:        "breaker-10-no-reports",
		breaker:      NewBreaker(BreakerParams{QueueDepth: 10, MaxConcurrency: 10, InitialCapacity: 10}),
		reportPeriod: time.Hour,
	}, {
		label:        "breaker-infinite-no-reports",
		breaker:      nil,
		reportPeriod: time.Hour,
	}, {
		label:        "breaker-10-many-reports",
		breaker:      NewBreaker(BreakerParams{QueueDepth: 10, MaxConcurrency: 10, InitialCapacity: 10}),
		reportPeriod: time.Microsecond,
	}, {
		label:        "breaker-infinite-many-reports",
		breaker:      nil,
		reportPeriod: time.Microsecond,
	}}

	for _, tc := range tests {
		exporter := tracetest.NewInMemoryExporter()
		tp := trace.NewTracerProvider(trace.WithSyncer(exporter))
		tracer := tp.Tracer("test")
		reportTicker := time.NewTicker(tc.reportPeriod)

		h := ProxyHandler(tracer, tc.breaker, stats, baseHandler)
		b.Run("sequential-"+tc.label, func(b *testing.B) {
			resp := httptest.NewRecorder()
			for range b.N {
				h(resp, req)
			}
		})
		b.Run("parallel-"+tc.label, func(b *testing.B) {
			b.RunParallel(func(pb *testing.PB) {
				resp := httptest.NewRecorder()
				for pb.Next() {
					h(resp, req)
				}
			})
		})

		reportTicker.Stop()
	}
}
