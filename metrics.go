package main

import (
	"net/http"
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	startTime           = time.Now()
	httpRequestCounters = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "filebrowser_http_requests_total", Help: "Total number of HTTP requests",
	}, []string{"status"})
	httpRequestsTotal   = httpRequestCounters.WithLabelValues("total")
	httpRequestsSuccess = httpRequestCounters.WithLabelValues("success")
	httpRequestsError   = httpRequestCounters.WithLabelValues("error")
	uploadCounters      = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "filebrowser_uploads_total", Help: "Total number of file uploads",
	}, []string{"status"})
	uploadsTotal      = uploadCounters.WithLabelValues("total")
	uploadsSuccess    = uploadCounters.WithLabelValues("success")
	uploadsError      = uploadCounters.WithLabelValues("error")
	resumableCounters = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "filebrowser_resumable_uploads_total", Help: "Resumable upload activity",
	}, []string{"event"})
	resumableCreated   = resumableCounters.WithLabelValues("created")
	resumableCompleted = resumableCounters.WithLabelValues("completed")
	resumableCanceled  = resumableCounters.WithLabelValues("canceled")
	resumableAppends   = resumableCounters.WithLabelValues("append")
	operationCounters  = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "filebrowser_operations_total", Help: "Total number of file operations",
	}, []string{"type"})
	directoryLists  = operationCounters.WithLabelValues("directory_list")
	fileServes      = operationCounters.WithLabelValues("file_serve")
	requestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "filebrowser_http_request_duration_seconds", Help: "HTTP request duration in seconds",
		Buckets: []float64{0.1, 0.5, 1},
	}, []string{"method"})
	metricsRegistry    *prometheus.Registry
	metricsHTTPHandler http.Handler
)

func init() {
	metricsRegistry = newMetricsRegistry()
	metricsHTTPHandler = promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{})
}

func newMetricsRegistry() *prometheus.Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(httpRequestCounters, uploadCounters, resumableCounters, operationCounters, requestDuration)
	r.MustRegister(collectors.NewGoCollector(), legacyRuntimeMetrics{})
	r.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "filebrowser_uptime_seconds", Help: "Total uptime in seconds",
	}, func() float64 { return time.Since(startTime).Seconds() }))
	// Preserve the existing zero-valued histogram series before the first request.
	for _, method := range []string{"DELETE", "GET", "PUT", "POST", "PATCH"} {
		requestDuration.WithLabelValues(method)
	}
	return r
}

func recordRequestDuration(method string, duration float64) {
	// Bound label cardinality even when a client sends an arbitrary method.
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodDelete, http.MethodPatch, http.MethodOptions, http.MethodConnect, http.MethodTrace:
	default:
		method = "OTHER"
	}
	requestDuration.WithLabelValues(method).Observe(duration)
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	if !enableMetrics {
		http.Error(w, "Metrics are disabled", http.StatusForbidden)
		return
	}
	metricsHTTPHandler.ServeHTTP(w, r)
}

// Retain the filebrowser runtime series for existing dashboards alongside the
// standard go_* metrics. The Prometheus client handles their serialization.
type legacyRuntimeMetrics struct{}

var (
	infoDesc       = prometheus.NewDesc("filebrowser_info", "Information about the file browser", []string{"version", "build_date"}, nil)
	memoryDesc     = prometheus.NewDesc("filebrowser_memory_bytes", "Memory usage in bytes", []string{"type"}, nil)
	goroutinesDesc = prometheus.NewDesc("filebrowser_goroutines", "Current number of goroutines", nil, nil)
	gcDesc         = prometheus.NewDesc("filebrowser_gc_total", "Total number of garbage collections", nil, nil)
	configDesc     = prometheus.NewDesc("filebrowser_config", "Configuration settings", []string{"setting"}, nil)
)

func (legacyRuntimeMetrics) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{infoDesc, memoryDesc, goroutinesDesc, gcDesc, configDesc} {
		ch <- desc
	}
}

func (legacyRuntimeMetrics) Collect(ch chan<- prometheus.Metric) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	ch <- prometheus.MustNewConstMetric(infoDesc, prometheus.GaugeValue, 1, GitCommit, BuildDate)
	for _, entry := range []struct {
		label string
		value uint64
	}{
		{"alloc", m.Alloc}, {"sys", m.Sys}, {"heap_alloc", m.HeapAlloc}, {"heap_sys", m.HeapSys},
	} {
		ch <- prometheus.MustNewConstMetric(memoryDesc, prometheus.GaugeValue, float64(entry.value), entry.label)
	}
	ch <- prometheus.MustNewConstMetric(goroutinesDesc, prometheus.GaugeValue, float64(runtime.NumGoroutine()))
	ch <- prometheus.MustNewConstMetric(gcDesc, prometheus.CounterValue, float64(m.NumGC))
	setting := "uploads_disabled"
	if enableUpload {
		setting = "uploads_enabled"
	}
	ch <- prometheus.MustNewConstMetric(configDesc, prometheus.GaugeValue, 1, setting)
}
