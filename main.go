package main

import (
	_ "embed"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	filesDir = "./files"
)

var (
	port    string
	address string
)

type FileInfo struct {
	Name         string
	Size         string
	LastModified string
	IsDir        bool
	URL          string
	SizeBytes    int64 // raw size (recursive for dirs) for client-side sorting
	ModUnix      int64 // mod time as unix seconds for client-side sorting
}

// Breadcrumb segment for the current path
type Crumb struct {
	Label string
	URL   string
}

var (
	title         = getEnv("TITLE", "File Server")
	extraHeaders  = getEnv("EXTRA_HEADERS", "")
	enableUpload  = getBoolEnv("ENABLE_UPLOAD", false)
	enableMetrics = getBoolEnv("ENABLE_METRICS", false)
	// Build information - set via ldflags during build
	GitCommit = "unknown"
	BuildDate = "unknown"
	// Metrics
	startTime           = time.Now()
	httpRequestsTotal   atomic.Uint64
	httpRequestsSuccess atomic.Uint64
	httpRequestsError   atomic.Uint64
	uploadsTotal        atomic.Uint64
	uploadsSuccess      atomic.Uint64
	uploadsError        atomic.Uint64
	directoryLists      atomic.Uint64
	fileServes          atomic.Uint64
	// Resumable upload metrics (draft-ietf-httpbis-resumable-upload-11)
	resumableCreated   atomic.Uint64
	resumableCompleted atomic.Uint64
	resumableCanceled  atomic.Uint64
	resumableAppends   atomic.Uint64

	requestDurationBuckets = map[string]map[string]*atomic.Uint64{
		"GET": {
			"0.1":  &atomic.Uint64{},
			"0.5":  &atomic.Uint64{},
			"1.0":  &atomic.Uint64{},
			"+Inf": &atomic.Uint64{},
		},
		"PUT": {
			"0.1":  &atomic.Uint64{},
			"0.5":  &atomic.Uint64{},
			"1.0":  &atomic.Uint64{},
			"+Inf": &atomic.Uint64{},
		},
		"POST": {
			"0.1":  &atomic.Uint64{},
			"0.5":  &atomic.Uint64{},
			"1.0":  &atomic.Uint64{},
			"+Inf": &atomic.Uint64{},
		},
		"PATCH": {
			"0.1":  &atomic.Uint64{},
			"0.5":  &atomic.Uint64{},
			"1.0":  &atomic.Uint64{},
			"+Inf": &atomic.Uint64{},
		},
	}
	requestDurationSum = map[string]*atomic.Uint64{
		"GET":   &atomic.Uint64{},
		"PUT":   &atomic.Uint64{},
		"POST":  &atomic.Uint64{},
		"PATCH": &atomic.Uint64{},
	}
	requestDurationCount = map[string]*atomic.Uint64{
		"GET":   &atomic.Uint64{},
		"PUT":   &atomic.Uint64{},
		"POST":  &atomic.Uint64{},
		"PATCH": &atomic.Uint64{},
	}
)

func main() {
	var enableUploadFlag bool
	var enableMetricsFlag bool
	var portFlag string
	var addressFlag string

	flag.BoolVar(&enableUploadFlag, "enable-upload", false, "Enable file uploads")
	flag.BoolVar(&enableMetricsFlag, "enable-metrics", false, "Enable metrics endpoint")
	flag.StringVar(&portFlag, "port", "8000", "Port to bind the server")
	flag.StringVar(&addressFlag, "address", "localhost", "Address to bind the server")
	flag.Parse()

	// Health check command
	if len(os.Args) > 1 && os.Args[1] == "health" {
		resp, err := http.Get("http://" + addressFlag + ":" + portFlag)
		if err != nil {
			os.Exit(1)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			os.Exit(0)
		}
		os.Exit(1)
	}

	port = ":" + portFlag
	address = addressFlag

	if enableUploadFlag {
		enableUpload = true
	}

	if enableMetricsFlag {
		enableMetrics = true
	}

	os.MkdirAll(filesDir, os.ModePerm)

	if enableUpload {
		// Resumable uploads keep in-memory state only, so partials from a
		// previous run are useless: wipe them and start a janitor to expire
		// abandoned uploads.
		wipeUploadDir()
		startUploadJanitor(registry, uploadMaxAge)
	}

	http.HandleFunc("/", pathHandler)
	http.HandleFunc("/metrics", metricsHandler)

	log.Printf("Server running at http://%s%s", address, port)

	if enableUpload {
		log.Printf("File uploads are enabled")
	} else {
		log.Printf("File uploads are disabled")
	}

	if enableMetrics {
		log.Printf("Metrics endpoint available at /metrics")
	} else {
		log.Printf("Metrics endpoint is disabled")
	}

	log.Fatal(http.ListenAndServe(port, nil))
}

func pathHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		if r.URL.Path != "/metrics" {
			duration := time.Since(start).Seconds()
			recordRequestDuration(r.Method, duration)
		}
	}()

	if r.URL.Path != "/metrics" {
		httpRequestsTotal.Add(1)
	}

	// Resumable upload resources live under a reserved prefix and are handled
	// before any file logic (spec: draft-ietf-httpbis-resumable-upload-11).
	if strings.HasPrefix(r.URL.Path, uploadURLPrefix) {
		handleUploadResource(w, r)
		return
	}

	switch r.Method {
	case http.MethodOptions:
		handleOptions(w, r)
		return
	case http.MethodPut:
		handlePutUpload(w, r)
		return
	case http.MethodPost:
		// POST is only meaningful as a resumable upload creation request.
		if hasUploadCreation(r) {
			handleUploadCreation(w, r)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	urlPath := r.URL.Path
	fullPath := filepath.Join(filesDir, urlPath)

	absFilesDir, _ := filepath.Abs(filesDir)
	absPath, _ := filepath.Abs(fullPath)
	if !strings.HasPrefix(absPath, absFilesDir) {
		if r.URL.Path != "/metrics" {
			httpRequestsError.Add(1)
		}
		http.NotFound(w, r)
		return
	}

	if _, err := os.Stat(filesDir); os.IsNotExist(err) {
		if r.URL.Path != "/metrics" {
			httpRequestsError.Add(1)
		}
		http.Error(w, fmt.Sprintf("files dir %s: no such directory", filesDir), http.StatusInternalServerError)
		return
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		if r.URL.Path != "/metrics" {
			httpRequestsError.Add(1)
		}
		if urlPath == "/" {
			http.Error(w, fmt.Sprintf("files dir %s: inaccessible or bad perms", filesDir), http.StatusInternalServerError)
		} else {
			http.Error(w, fmt.Sprintf("%s: no such file or directory", fullPath), http.StatusNotFound)
		}
		return
	}

	if info.IsDir() {
		if !strings.HasSuffix(urlPath, "/") {
			http.Redirect(w, r, urlPath+"/", http.StatusFound)
			return
		}
		directoryLists.Add(1)
		listDirectory(w, fullPath, urlPath)
	} else {
		fileServes.Add(1)
		http.ServeFile(w, r, fullPath)
	}

	if r.URL.Path != "/metrics" {
		httpRequestsSuccess.Add(1)
	}
}

func listDirectory(w http.ResponseWriter, dirPath string, urlPath string) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		http.Error(w, "Error reading directory", http.StatusInternalServerError)
		return
	}

	parentURL := "/"
	if urlPath != "/" {
		parts := strings.Split(strings.Trim(urlPath, "/"), "/")
		if len(parts) > 1 {
			parentURL = "/" + strings.Join(parts[:len(parts)-1], "/") + "/"
		} else if len(parts) == 1 {
			parentURL = "/"
		}
	}

	fileInfos := make([]FileInfo, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}

		entryURL := entry.Name()
		var size string
		var sizeBytes int64
		if entry.IsDir() {
			entryURL += "/"
			subDirPath := filepath.Join(dirPath, entry.Name())
			bytes, complete := cachedDirSize(subDirPath, info.ModTime())
			sizeBytes = bytes
			size = formatSize(bytes)
			if !complete {
				size = "~" + size // partial scan (timed out): lower-bound estimate
			}
		} else {
			sizeBytes = info.Size()
			size = formatSize(sizeBytes)
		}

		fileInfos = append(fileInfos, FileInfo{
			Name:         entry.Name(),
			Size:         size,
			LastModified: info.ModTime().Format("2006-01-02 15:04-07:00"),
			IsDir:        entry.IsDir(),
			URL:          entryURL,
			SizeBytes:    sizeBytes,
			ModUnix:      info.ModTime().Unix(),
		})
	}

	sort.Slice(fileInfos, func(i, j int) bool {
		if fileInfos[i].IsDir && !fileInfos[j].IsDir {
			return true
		}
		if !fileInfos[i].IsDir && fileInfos[j].IsDir {
			return false
		}
		return fileInfos[i].Name < fileInfos[j].Name
	})

	tmpl := template.New("index").Funcs(template.FuncMap{
		"safeHTML": func(s string) template.HTML {
			return template.HTML(s)
		},
	})

	tmpl, err = tmpl.Parse(htmlTemplate)
	if err != nil {
		http.Error(w, "Error rendering page", http.StatusInternalServerError)
		return
	}

	breadcrumbs := buildBreadcrumbs(urlPath)

	data := struct {
		CurrentPath   string
		ParentURL     string
		Files         []FileInfo
		Title         string
		ExtraHeaders  string
		GitCommit     string
		BuildDate     string
		DisableUpload bool
		Breadcrumbs   []Crumb
	}{
		CurrentPath:   urlPath,
		ParentURL:     parentURL,
		Files:         fileInfos,
		Title:         title,
		ExtraHeaders:  extraHeaders,
		GitCommit:     GitCommit,
		BuildDate:     BuildDate,
		DisableUpload: !enableUpload,
		Breadcrumbs:   breadcrumbs,
	}

	tmpl.Execute(w, data)
}

func handlePutUpload(w http.ResponseWriter, r *http.Request) {
	// A PUT carrying an Upload-Complete header is a resumable upload creation
	// request (transparent upgrade, spec §12.1.1). A plain PUT keeps the
	// original single-shot behavior below for full backward compatibility.
	if hasUploadCreation(r) {
		handleUploadCreation(w, r)
		return
	}

	uploadsTotal.Add(1)

	if !enableUpload {
		uploadsError.Add(1)
		httpRequestsError.Add(1)
		http.Error(w, "File uploads are disabled", http.StatusForbidden)
		return
	}

	urlPath := r.URL.Path
	fullPath := filepath.Join(filesDir, urlPath)

	absFilesDir, _ := filepath.Abs(filesDir)
	absPath, _ := filepath.Abs(fullPath)
	if !strings.HasPrefix(absPath, absFilesDir) {
		uploadsError.Add(1)
		httpRequestsError.Add(1)
		http.Error(w, "Invalid file path", http.StatusForbidden)
		return
	}

	if strings.HasSuffix(urlPath, "/") {
		uploadsError.Add(1)
		httpRequestsError.Add(1)
		http.Error(w, "Cannot upload to a directory path", http.StatusBadRequest)
		return
	}

	// Create parent directory if it doesn't exist
	parentDir := filepath.Dir(fullPath)
	if err := os.MkdirAll(parentDir, os.ModePerm); err != nil {
		uploadsError.Add(1)
		httpRequestsError.Add(1)
		http.Error(w, "Unable to create directory", http.StatusInternalServerError)
		return
	}

	// Create or overwrite the file
	dst, err := os.Create(fullPath)
	if err != nil {
		uploadsError.Add(1)
		httpRequestsError.Add(1)
		http.Error(w, "Unable to create file", http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	// Copy the request body to the file
	_, err = io.Copy(dst, r.Body)
	if err != nil {
		uploadsError.Add(1)
		httpRequestsError.Add(1)
		http.Error(w, "Error saving file", http.StatusInternalServerError)
		return
	}

	invalidateDirSizes(fullPath)

	uploadsSuccess.Add(1)
	httpRequestsSuccess.Add(1)
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "File uploaded successfully to %s\n", urlPath)
}

func recordRequestDuration(method string, duration float64) {
	buckets, exists := requestDurationBuckets[method]
	if !exists {
		return
	}

	requestDurationCount[method].Add(1)
	durationMicros := uint64(duration * 1_000_000)
	requestDurationSum[method].Add(durationMicros)

	bucketThresholds := []string{"0.1", "0.5", "1.0", "+Inf"}
	thresholds := []float64{0.1, 0.5, 1.0, math.Inf(1)}

	for i, threshold := range thresholds {
		if duration <= threshold {
			buckets[bucketThresholds[i]].Add(1)
		}
	}
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	if !enableMetrics {
		http.Error(w, "Metrics are disabled", http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	uptime := time.Since(startTime).Seconds()

	fmt.Fprintf(w, "# HELP filebrowser_info Information about the file browser\n")
	fmt.Fprintf(w, "# TYPE filebrowser_info gauge\n")
	fmt.Fprintf(w, "filebrowser_info{version=\"%s\",build_date=\"%s\"} 1\n", GitCommit, BuildDate)
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "# HELP filebrowser_uptime_seconds Total uptime in seconds\n")
	fmt.Fprintf(w, "# TYPE filebrowser_uptime_seconds gauge\n")
	fmt.Fprintf(w, "filebrowser_uptime_seconds %.2f\n", uptime)
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "# HELP filebrowser_http_requests_total Total number of HTTP requests\n")
	fmt.Fprintf(w, "# TYPE filebrowser_http_requests_total counter\n")
	fmt.Fprintf(w, "filebrowser_http_requests_total{status=\"total\"} %d\n", httpRequestsTotal.Load())
	fmt.Fprintf(w, "filebrowser_http_requests_total{status=\"success\"} %d\n", httpRequestsSuccess.Load())
	fmt.Fprintf(w, "filebrowser_http_requests_total{status=\"error\"} %d\n", httpRequestsError.Load())
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "# HELP filebrowser_uploads_total Total number of file uploads\n")
	fmt.Fprintf(w, "# TYPE filebrowser_uploads_total counter\n")
	fmt.Fprintf(w, "filebrowser_uploads_total{status=\"total\"} %d\n", uploadsTotal.Load())
	fmt.Fprintf(w, "filebrowser_uploads_total{status=\"success\"} %d\n", uploadsSuccess.Load())
	fmt.Fprintf(w, "filebrowser_uploads_total{status=\"error\"} %d\n", uploadsError.Load())
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "# HELP filebrowser_resumable_uploads_total Resumable upload activity\n")
	fmt.Fprintf(w, "# TYPE filebrowser_resumable_uploads_total counter\n")
	fmt.Fprintf(w, "filebrowser_resumable_uploads_total{event=\"created\"} %d\n", resumableCreated.Load())
	fmt.Fprintf(w, "filebrowser_resumable_uploads_total{event=\"completed\"} %d\n", resumableCompleted.Load())
	fmt.Fprintf(w, "filebrowser_resumable_uploads_total{event=\"canceled\"} %d\n", resumableCanceled.Load())
	fmt.Fprintf(w, "filebrowser_resumable_uploads_total{event=\"append\"} %d\n", resumableAppends.Load())
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "# HELP filebrowser_operations_total Total number of file operations\n")
	fmt.Fprintf(w, "# TYPE filebrowser_operations_total counter\n")
	fmt.Fprintf(w, "filebrowser_operations_total{type=\"directory_list\"} %d\n", directoryLists.Load())
	fmt.Fprintf(w, "filebrowser_operations_total{type=\"file_serve\"} %d\n", fileServes.Load())
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "# HELP filebrowser_http_request_duration_seconds HTTP request duration in seconds\n")
	fmt.Fprintf(w, "# TYPE filebrowser_http_request_duration_seconds histogram\n")

	for method, buckets := range requestDurationBuckets {
		for bucket, count := range buckets {
			fmt.Fprintf(w, "filebrowser_http_request_duration_seconds_bucket{le=\"%s\",method=\"%s\"} %d\n", bucket, method, count.Load())
		}

		sumMicros := requestDurationSum[method].Load()
		sumSeconds := float64(sumMicros) / 1_000_000
		fmt.Fprintf(w, "filebrowser_http_request_duration_seconds_sum{method=\"%s\"} %.6f\n", method, sumSeconds)
		fmt.Fprintf(w, "filebrowser_http_request_duration_seconds_count{method=\"%s\"} %d\n", method, requestDurationCount[method].Load())
	}
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "# HELP filebrowser_memory_bytes Memory usage in bytes\n")
	fmt.Fprintf(w, "# TYPE filebrowser_memory_bytes gauge\n")
	fmt.Fprintf(w, "filebrowser_memory_bytes{type=\"alloc\"} %d\n", m.Alloc)
	fmt.Fprintf(w, "filebrowser_memory_bytes{type=\"sys\"} %d\n", m.Sys)
	fmt.Fprintf(w, "filebrowser_memory_bytes{type=\"heap_alloc\"} %d\n", m.HeapAlloc)
	fmt.Fprintf(w, "filebrowser_memory_bytes{type=\"heap_sys\"} %d\n", m.HeapSys)
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "# HELP filebrowser_goroutines Current number of goroutines\n")
	fmt.Fprintf(w, "# TYPE filebrowser_goroutines gauge\n")
	fmt.Fprintf(w, "filebrowser_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "# HELP filebrowser_gc_total Total number of garbage collections\n")
	fmt.Fprintf(w, "# TYPE filebrowser_gc_total counter\n")
	fmt.Fprintf(w, "filebrowser_gc_total %d\n", m.NumGC)
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "# HELP filebrowser_config Configuration settings\n")
	fmt.Fprintf(w, "# TYPE filebrowser_config gauge\n")
	if enableUpload {
		fmt.Fprintf(w, "filebrowser_config{setting=\"uploads_enabled\"} 1\n")
	} else {
		fmt.Fprintf(w, "filebrowser_config{setting=\"uploads_disabled\"} 1\n")
	}
}

// ---- Directory size estimation (cached) ----

const dirScanBudget = 100 * time.Millisecond // per-directory walk time budget

type dirSizeEntry struct {
	bytes    int64
	complete bool
	mtime    time.Time
}

// dirSizeCache maps a directory path to its last computed recursive size.
var dirSizeCache sync.Map

// cachedDirSize returns the (approximate) recursive byte size of a directory,
// caching the result. A cached entry is reused until either the directory's
// own mtime changes (an out-of-band add/remove of a direct child) or an upload
// explicitly clears it via invalidateDirSizes. There is no time-based expiry:
// this app is the only writer that changes nested totals, and it invalidates
// the affected directories itself.
func cachedDirSize(path string, mtime time.Time) (int64, bool) {
	if v, ok := dirSizeCache.Load(path); ok {
		e := v.(dirSizeEntry)
		if e.mtime.Equal(mtime) {
			return e.bytes, e.complete
		}
	}
	bytes, complete, _ := ApproxDirSize(path, dirScanBudget)
	dirSizeCache.Store(path, dirSizeEntry{bytes: bytes, complete: complete, mtime: mtime})
	return bytes, complete
}

// invalidateDirSizes drops the cached size for the directory containing
// fullPath and every ancestor up to (and including) filesDir. An upload
// increases the recursive total of every directory above it, and overwriting
// an existing file changes those totals without changing any directory's
// mtime, so the cache must be cleared explicitly whenever a file is written.
func invalidateDirSizes(fullPath string) {
	absFiles, err := filepath.Abs(filesDir)
	if err != nil {
		return
	}
	dir := filepath.Dir(fullPath)
	for {
		dirSizeCache.Delete(dir)
		absDir, err := filepath.Abs(dir)
		if err != nil || absDir == absFiles {
			return
		}
		if !strings.HasPrefix(absDir, absFiles+string(os.PathSeparator)) {
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

// ApproxDirSize sums the sizes of all files under path, giving up after
// maxDuration so a huge tree can't stall a page render. complete is false when
// the walk was cut short, in which case bytes is a lower-bound estimate.
//
// Note: filepath.WalkDir turns a SkipAll return into a nil error, so the
// timeout is tracked with an explicit flag rather than inferred from err.
func ApproxDirSize(path string, maxDuration time.Duration) (bytes int64, complete bool, err error) {
	deadline := time.Now().Add(maxDuration)
	timedOut := false

	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // ignore unreadable files for UI estimates
		}
		if time.Now().After(deadline) {
			timedOut = true
			return filepath.SkipAll
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		bytes += info.Size()
		return nil
	})

	complete = err == nil && !timedOut
	return bytes, complete, err
}

func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func getBoolEnv(key string, defaultValue bool) bool {
	if value, exists := os.LookupEnv(key); exists {
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}

func buildBreadcrumbs(urlPath string) []Crumb {
	crumbs := []Crumb{{Label: "/", URL: "/"}}
	clean := strings.Trim(urlPath, "/")
	if clean == "" {
		return crumbs
	}
	parts := strings.Split(clean, "/")
	current := "/"
	for _, p := range parts {
		current += p + "/"
		crumbs = append(crumbs, Crumb{Label: p + "/", URL: current})
	}
	return crumbs
}

//go:embed template.html
var htmlTemplate string
