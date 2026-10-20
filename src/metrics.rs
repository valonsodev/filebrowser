use prometheus::{
    CounterVec, Encoder, Gauge, GaugeVec, HistogramOpts, HistogramVec, Opts, Registry, TextEncoder,
};
use std::time::Instant;

pub struct Metrics {
    registry: Registry,
    pub requests: CounterVec,
    pub uploads: CounterVec,
    pub resumable: CounterVec,
    pub operations: CounterVec,
    duration: HistogramVec,
    uptime: Gauge,
    started: Instant,
}
impl Metrics {
    pub fn new(uploads: bool) -> Self {
        let registry = Registry::new();
        let counter = |name, help, label| {
            let metric = CounterVec::new(Opts::new(name, help), &[label]).unwrap();
            registry.register(Box::new(metric.clone())).unwrap();
            metric
        };
        let requests = counter(
            "filebrowser_http_requests_total",
            "Total number of HTTP requests",
            "status",
        );
        let upload_count = counter(
            "filebrowser_uploads_total",
            "Total number of file uploads",
            "status",
        );
        let resumable = counter(
            "filebrowser_resumable_uploads_total",
            "Resumable upload activity",
            "event",
        );
        let operations = counter(
            "filebrowser_operations_total",
            "Total number of file operations",
            "type",
        );
        for status in ["total", "success", "error"] {
            let _ = requests.with_label_values(&[status]);
            let _ = upload_count.with_label_values(&[status]);
        }
        for event in ["created", "completed", "canceled", "append"] {
            let _ = resumable.with_label_values(&[event]);
        }
        for operation in ["directory_list", "file_serve"] {
            let _ = operations.with_label_values(&[operation]);
        }
        let duration = HistogramVec::new(
            HistogramOpts::new(
                "filebrowser_http_request_duration_seconds",
                "HTTP request duration in seconds",
            )
            .buckets(vec![0.1, 0.5, 1.0]),
            &["method"],
        )
        .unwrap();
        for method in ["DELETE", "GET", "PUT", "POST", "PATCH"] {
            let _ = duration.with_label_values(&[method]);
        }
        registry.register(Box::new(duration.clone())).unwrap();
        let uptime = Gauge::new("filebrowser_uptime_seconds", "Total uptime in seconds").unwrap();
        registry.register(Box::new(uptime.clone())).unwrap();
        let info = GaugeVec::new(
            Opts::new("filebrowser_info", "Information about the file browser"),
            &["version", "build_date"],
        )
        .unwrap();
        info.with_label_values(&[env!("GIT_COMMIT"), env!("BUILD_DATE")])
            .set(1.0);
        registry.register(Box::new(info)).unwrap();
        let config = GaugeVec::new(
            Opts::new("filebrowser_config", "Configuration settings"),
            &["setting"],
        )
        .unwrap();
        config
            .with_label_values(&[if uploads {
                "uploads_enabled"
            } else {
                "uploads_disabled"
            }])
            .set(1.0);
        registry.register(Box::new(config)).unwrap();
        registry
            .register(Box::new(
                prometheus::process_collector::ProcessCollector::for_self(),
            ))
            .unwrap();
        Self {
            registry,
            requests,
            uploads: upload_count,
            resumable,
            operations,
            duration,
            uptime,
            started: Instant::now(),
        }
    }
    pub fn record_request(&self, success: bool) {
        self.requests.with_label_values(&["total"]).inc();
        self.requests
            .with_label_values(&[if success { "success" } else { "error" }])
            .inc();
    }
    pub fn observe_duration(&self, method: &str, seconds: f64) {
        let method = match method {
            "GET" | "HEAD" | "PUT" | "POST" | "PATCH" | "DELETE" | "OPTIONS" | "CONNECT"
            | "TRACE" => method,
            _ => "OTHER",
        };
        self.duration.with_label_values(&[method]).observe(seconds);
    }
    pub fn encode(&self) -> Result<Vec<u8>, prometheus::Error> {
        self.uptime.set(self.started.elapsed().as_secs_f64());
        let mut out = Vec::new();
        TextEncoder::new().encode(&self.registry.gather(), &mut out)?;
        Ok(out)
    }
}
