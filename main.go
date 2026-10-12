package main

import (
	_ "embed"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	ExpiresUnix  int64
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

	if err := loadExpirations(); err != nil {
		log.Fatalf("Loading file expirations: %v", err)
	}
	startFileJanitor()

	if enableUpload {
		if err := loadUploads(registry); err != nil {
			log.Fatalf("Loading resumable uploads: %v", err)
		}
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

	log.Fatal(http.ListenAndServe(net.JoinHostPort(address, portFlag), nil))
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

	if err := browserOriginProtection.Check(r); err != nil {
		httpRequestsError.Add(1)
		http.Error(w, "Cross-origin file changes are forbidden", http.StatusForbidden)
		return
	}

	// Resumable upload resources live under a reserved prefix and are handled
	// before any file logic (spec: draft-ietf-httpbis-resumable-upload-11).
	if strings.HasPrefix(r.URL.Path, uploadURLPrefix) {
		handleUploadResource(w, r)
		return
	}

	if reservedFilePath(r.URL.Path) {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodDelete, http.MethodPatch:
		handleFileAction(w, r)
		return
	case http.MethodOptions:
		handleOptions(w, r)
		return
	case http.MethodPut:
		handlePutUpload(w, r)
		return
	case http.MethodPost:
		if hasUploadCreation(r) {
			handleUploadCreation(w, r)
		} else if r.URL.Query().Get("folder") == "true" {
			handleCreateFolder(w, r)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	urlPath := r.URL.Path
	rel, err := userPath(urlPath)
	if err != nil {
		httpRequestsError.Add(1)
		http.Error(w, "Invalid file path", http.StatusForbidden)
		return
	}
	root, err := os.OpenRoot(filesDir)
	if err != nil {
		httpRequestsError.Add(1)
		http.Error(w, "Unable to access files", http.StatusInternalServerError)
		return
	}
	defer root.Close()
	entry, err := root.Lstat(rel)
	if err != nil {
		httpRequestsError.Add(1)
		http.NotFound(w, r)
		return
	}
	if !entry.IsDir() && !entry.Mode().IsRegular() {
		httpRequestsError.Add(1)
		http.Error(w, "Only regular files and directories can be accessed", http.StatusForbidden)
		return
	}
	f, err := root.Open(rel)
	if err != nil {
		httpRequestsError.Add(1)
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		httpRequestsError.Add(1)
		http.Error(w, "Unable to access file", http.StatusInternalServerError)
		return
	}
	fullPath := filepath.Join(filesDir, rel)
	if info.IsDir() {
		if !strings.HasSuffix(urlPath, "/") {
			target := *r.URL
			target.Path += "/"
			target.RawPath = ""
			http.Redirect(w, r, target.RequestURI(), http.StatusFound)
			return
		}
		directoryLists.Add(1)
		listDirectory(w, r, fullPath, urlPath)
	} else {
		fileServes.Add(1)
		w.Header().Set("Content-Disposition", fileDisposition(info.Name()))
		if !info.Mode().IsRegular() {
			httpRequestsError.Add(1)
			http.Error(w, "Only regular files can be served", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, info.Name(), info.ModTime(), f)
	}

	if r.URL.Path != "/metrics" {
		httpRequestsSuccess.Add(1)
	}
}

func fileDisposition(name string) string {
	// Keep a plain filename for curl and an encoded filename for browsers.
	// Strip control characters so filesystem names cannot inject HTTP headers.
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r == '/' || r == '\\' {
			return -1
		}
		return r
	}, name)
	if name == "" {
		name = "download"
	}
	quoted := strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(name)
	encoded := strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
	return "attachment; filename=\"" + quoted + "\"; filename*=UTF-8''" + encoded
}

func listDirectory(w http.ResponseWriter, r *http.Request, dirPath string, urlPath string) {
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
		if entry.Name() == metadataDir {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			continue
		}

		entryURL := "./" + url.PathEscape(entry.Name())
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
			ExpiresUnix:  expiryUnix(filepath.Join(dirPath, entry.Name())),
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

	w.Header().Add("Vary", "Accept")
	w.Header().Add("Vary", "User-Agent")
	w.Header().Add("Link", "<"+r.URL.EscapedPath()+">; rel=\"alternate\"; type=\"text/markdown\"")
	if wantsMarkdown(r) {
		writeMarkdownDirectory(w, r, urlPath, parentURL, fileInfos)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}

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
		CurrentPath      string
		ParentURL        string
		Files            []FileInfo
		Title            string
		ExtraHeaders     string
		GitCommit        string
		BuildDate        string
		DisableUpload    bool
		Breadcrumbs      []Crumb
		UploadHashWorker string
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
	if enableUpload {
		data.UploadHashWorker = uploadHashLibrary + "\n" + uploadHashWorker
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

	temporary, err := temporaryUpload(r)
	if err != nil {
		uploadsError.Add(1)
		httpRequestsError.Add(1)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	urlPath := r.URL.Path
	target, fullPath, ok := resolveTarget(w, urlPath)
	if !ok {
		uploadsError.Add(1)
		httpRequestsError.Add(1)
		return
	}
	dst, err := os.CreateTemp(uploadsDir, "put-")
	if err != nil {
		uploadsError.Add(1)
		httpRequestsError.Add(1)
		http.Error(w, "Unable to create upload", http.StatusInternalServerError)
		return
	}
	defer os.Remove(dst.Name())
	_, copyErr := io.Copy(dst, r.Body)
	closeErr := dst.Close()
	if copyErr != nil || closeErr != nil {
		uploadsError.Add(1)
		httpRequestsError.Add(1)
		http.Error(w, "Error saving file", http.StatusInternalServerError)
		return
	}
	fileActions.Lock()
	defer fileActions.Unlock()
	if err := installUploadedFile(dst.Name(), target, temporary); err != nil {
		uploadsError.Add(1)
		httpRequestsError.Add(1)
		http.Error(w, "Unable to save file", http.StatusForbidden)
		return
	}
	invalidateDirSizes(fullPath)
	if expiry, ok := fileActions.expirations[target]; ok {
		w.Header().Set("File-Expires-At", strconv.FormatInt(expiry.ExpiresAt.Unix(), 10))
	}

	uploadsSuccess.Add(1)
	httpRequestsSuccess.Add(1)
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "File uploaded successfully to %s\n", urlPath)
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

//go:embed assets/js-sha256.min.js
var uploadHashLibrary string

//go:embed assets/upload-hash-worker.js
var uploadHashWorker string
