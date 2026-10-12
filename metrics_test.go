package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

func TestMetricsEndpoint(t *testing.T) {
	oldEnabled, oldCommit, oldBuild := enableMetrics, GitCommit, BuildDate
	t.Cleanup(func() { enableMetrics, GitCommit, BuildDate = oldEnabled, oldCommit, oldBuild })
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	enableMetrics = false
	w := httptest.NewRecorder()
	metricsHandler(w, request)
	if w.Code != http.StatusForbidden {
		t.Fatalf("disabled metrics status = %d", w.Code)
	}

	enableMetrics = true
	GitCommit, BuildDate = "commit\"\\\nvalue", "date\"\\\nvalue"
	w = httptest.NewRecorder()
	metricsHandler(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("metrics status = %d: %s", w.Code, w.Body.String())
	}
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(w.Body.String()))
	if err != nil {
		t.Fatalf("invalid Prometheus exposition: %v", err)
	}
	for _, name := range []string{
		"filebrowser_info", "filebrowser_uptime_seconds", "filebrowser_http_requests_total",
		"filebrowser_uploads_total", "filebrowser_resumable_uploads_total", "filebrowser_operations_total",
		"filebrowser_http_request_duration_seconds", "filebrowser_memory_bytes", "filebrowser_goroutines",
		"filebrowser_gc_total", "filebrowser_config", "go_goroutines", "go_memstats_alloc_bytes", "go_info",
	} {
		if families[name] == nil {
			t.Errorf("missing metric %s", name)
		}
	}
	info := families["filebrowser_info"]
	if info == nil || len(info.Metric) != 1 {
		t.Fatal("missing build information")
	}
	labels := make(map[string]string)
	for _, label := range info.Metric[0].Label {
		labels[label.GetName()] = label.GetValue()
	}
	if labels["version"] != GitCommit || labels["build_date"] != BuildDate {
		t.Fatalf("build labels did not round trip: %v", labels)
	}
}

func TestRequestDurationHistogram(t *testing.T) {
	// Compare against the current values because other tests also make requests.
	before, err := metricsRegistry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var count uint64
	var sum float64
	buckets := make([]uint64, 3)
	for _, family := range before {
		if family.GetName() != "filebrowser_http_request_duration_seconds" {
			continue
		}
		for _, metric := range family.Metric {
			if metric.Label[0].GetValue() == http.MethodGet {
				h := metric.GetHistogram()
				count, sum = h.GetSampleCount(), h.GetSampleSum()
				for i, bucket := range h.Bucket {
					buckets[i] = bucket.GetCumulativeCount()
				}
			}
		}
	}
	for _, duration := range []float64{0.05, 0.2, 0.8, 1.5} {
		recordRequestDuration(http.MethodGet, duration)
	}
	recordRequestDuration("CUSTOM-UNBOUNDED-METHOD", 0.2)
	families, err := metricsRegistry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	foundGET, foundOther := false, false
	for _, family := range families {
		if family.GetName() != "filebrowser_http_request_duration_seconds" {
			continue
		}
		for _, metric := range family.Metric {
			method := metric.Label[0].GetValue()
			if method == "OTHER" {
				foundOther = true
			}
			if method != http.MethodGet {
				continue
			}
			foundGET = true
			h := metric.GetHistogram()
			if h.GetSampleCount()-count != 4 || math.Abs(h.GetSampleSum()-sum-2.55) > 1e-9 {
				t.Fatalf("histogram count/sum = %d/%g", h.GetSampleCount(), h.GetSampleSum())
			}
			if len(h.Bucket) != 3 {
				t.Fatalf("bucket count = %d", len(h.Bucket))
			}
			for i, bound := range []float64{0.1, 0.5, 1} {
				if h.Bucket[i].GetUpperBound() != bound || h.Bucket[i].GetCumulativeCount()-buckets[i] != uint64(i+1) {
					t.Errorf("unexpected bucket %v", h.Bucket[i])
				}
			}
		}
	}
	if !foundGET || !foundOther {
		t.Fatal("missing GET or bounded OTHER histogram")
	}
}
