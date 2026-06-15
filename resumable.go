package main

// Implementation of the IETF draft "Resumable Uploads for HTTP"
// (draft-ietf-httpbis-resumable-upload-11). See the spec text in
// draft-ietf-httpbis-resumable-upload-11.txt in the repo root.
//
// An upload is modeled as a temporary "upload resource" (its own URI under
// uploadURLPrefix) that is separate from the final file. It tracks an offset,
// an optional length and an explicit completeness flag. Clients create it,
// query its offset with HEAD, append bytes with PATCH, and cancel with DELETE.
// On completion the assembled partial file is moved to its target path under
// filesDir.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// uploadsDir holds the partial upload data. It is a sibling of filesDir so
	// it is never listed or served.
	uploadsDir = "./.uploads"
	// uploadURLPrefix is the reserved URL namespace for upload resources.
	uploadURLPrefix = "/.uploads/"
	// partialUploadMediaType is the media type required on PATCH appends.
	partialUploadMediaType = "application/partial-upload"
	// uploadMaxAge is how long an upload resource (including a completed
	// tombstone) is kept before the janitor reclaims it.
	uploadMaxAge = 24 * time.Hour
	// chunkCopyOnFinalize is the buffer size for the cross-filesystem move
	// fallback.
	moveBufSize = 1 << 20

	// Problem type URNs from the draft (§7).
	problemMismatchingOffset = "https://iana.org/assignments/http-problem-types#mismatching-upload-offset"
	problemCompletedUpload   = "https://iana.org/assignments/http-problem-types#completed-upload"
	problemInconsistentLen   = "https://iana.org/assignments/http-problem-types#inconsistent-upload-length"
)

// uploadIDPattern validates an upload id taken from the URL before it is used
// to build a filesystem path. IDs are generated as 16 random bytes hex-encoded.
var uploadIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// upload is the server-side state of a single resumable upload.
type upload struct {
	id         string
	targetPath string // final path under filesDir, e.g. "sub/file.txt"

	mu          sync.Mutex // exclusive access-lock (spec §4.6)
	offset      int64
	length      int64
	lengthKnown bool
	complete    bool
	deleted     bool
	createdAt   time.Time
	updatedAt   time.Time
}

// uploadRegistry is an in-memory map of live upload resources.
type uploadRegistry struct {
	mu sync.Mutex
	m  map[string]*upload
}

func newUploadRegistry() *uploadRegistry {
	return &uploadRegistry{m: make(map[string]*upload)}
}

// registry is the process-wide upload registry, initialised in main().
var registry = newUploadRegistry()

func (r *uploadRegistry) get(id string) (*upload, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.m[id]
	return u, ok
}

// create allocates a new upload resource with an unguessable id.
func (r *uploadRegistry) create(targetPath string, length int64, lengthKnown bool) (*upload, error) {
	id, err := newUploadID()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	u := &upload{
		id:          id,
		targetPath:  targetPath,
		length:      length,
		lengthKnown: lengthKnown,
		createdAt:   now,
		updatedAt:   now,
	}
	r.mu.Lock()
	r.m[id] = u
	r.mu.Unlock()
	return u, nil
}

// remove deletes an entry from the registry map. It does not touch disk.
func (r *uploadRegistry) remove(id string) {
	r.mu.Lock()
	delete(r.m, id)
	r.mu.Unlock()
}

func newUploadID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (u *upload) partialPath() string {
	return filepath.Join(uploadsDir, u.id)
}

func (u *upload) location() string {
	return uploadURLPrefix + u.id
}

// ---- Startup / maintenance -------------------------------------------------

// wipeUploadDir removes any partial uploads left over from a previous run.
// In-memory state does not survive a restart, so the orphaned data is useless
// and the draft requires the server to consider such uploads gone.
//
// It clears the directory's contents but does NOT remove the directory itself:
// in restricted deployments (e.g. running as a non-root user whose parent
// directory is not writable) the process can write inside an existing uploads
// dir but cannot recreate it. It then verifies the dir is actually writable so
// a misconfigured mount fails at boot rather than silently breaking every
// upload (which previously surfaced as a request that hangs forever).
func wipeUploadDir() {
	if err := os.MkdirAll(uploadsDir, os.ModePerm); err != nil {
		log.Fatalf("uploads directory %q is not usable: %v", uploadsDir, err)
	}
	entries, err := os.ReadDir(uploadsDir)
	if err != nil {
		log.Fatalf("cannot read uploads directory %q: %v", uploadsDir, err)
	}
	for _, e := range entries {
		os.RemoveAll(filepath.Join(uploadsDir, e.Name()))
	}
	probe := filepath.Join(uploadsDir, ".write-probe")
	if err := os.WriteFile(probe, nil, 0o644); err != nil {
		log.Fatalf("uploads directory %q is not writable: %v", uploadsDir, err)
	}
	os.Remove(probe)
}

// startUploadJanitor periodically reclaims upload resources (including
// completed tombstones) older than maxAge.
func startUploadJanitor(r *uploadRegistry, maxAge time.Duration) {
	go func() {
		ticker := time.NewTicker(maxAge / 4)
		defer ticker.Stop()
		for range ticker.C {
			cutoff := time.Now().Add(-maxAge)
			var stale []*upload
			r.mu.Lock()
			for id, u := range r.m {
				if u.updatedAt.Before(cutoff) {
					stale = append(stale, u)
					delete(r.m, id)
				}
			}
			r.mu.Unlock()
			for _, u := range stale {
				u.mu.Lock()
				u.deleted = true
				os.Remove(u.partialPath())
				u.mu.Unlock()
			}
		}
	}()
}

// ---- Routing ---------------------------------------------------------------

// handleUploadResource dispatches requests to an existing upload resource
// (paths under uploadURLPrefix).
func handleUploadResource(w http.ResponseWriter, r *http.Request) {
	if !enableUpload {
		http.Error(w, "File uploads are disabled", http.StatusForbidden)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, uploadURLPrefix)
	if !uploadIDPattern.MatchString(id) {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodHead:
		headUpload(w, r, id)
	case http.MethodPatch:
		appendToUpload(w, r, id)
	case http.MethodDelete:
		cancelUpload(w, r, id)
	case http.MethodOptions:
		optionsUpload(w, r)
	default:
		w.Header().Set("Allow", "HEAD, PATCH, DELETE, OPTIONS")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---- Upload creation -------------------------------------------------------

// hasUploadCreation reports whether a request is an attempt to create a
// resumable upload (the transparent-upgrade / creation trigger is the presence
// of the Upload-Complete header, spec §4.2 / §12.1.1).
func hasUploadCreation(r *http.Request) bool {
	return r.Header.Get("Upload-Complete") != ""
}

// handleUploadCreation handles a creation request (POST/PUT with an
// Upload-Complete header) targeting a file path. It creates an upload
// resource, streams any included body into the partial file, and either
// finalizes (Upload-Complete: ?1 fully received) or leaves the resource open.
func handleUploadCreation(w http.ResponseWriter, r *http.Request) {
	if !enableUpload {
		writeProblem(w, http.StatusForbidden, "", "File uploads are disabled")
		return
	}
	uploadsTotal.Add(1)

	complete, ok := parseSFBool(r.Header.Get("Upload-Complete"))
	if !ok {
		uploadsError.Add(1)
		writeProblem(w, http.StatusBadRequest, "", "invalid Upload-Complete header")
		return
	}

	// Resolve and validate the target path the same way the legacy upload does.
	targetPath, _, ok := resolveTarget(w, r.URL.Path)
	if !ok {
		uploadsError.Add(1)
		return
	}

	// Determine declared length, if any (spec §4.1.3).
	length, lengthKnown, ok := creationLength(w, r, complete)
	if !ok {
		uploadsError.Add(1)
		return
	}

	u, err := registry.create(targetPath, length, lengthKnown)
	if err != nil {
		uploadsError.Add(1)
		writeProblem(w, http.StatusInternalServerError, "", "unable to create upload")
		return
	}
	resumableCreated.Add(1)

	// Best-effort 104 (Upload Resumption Supported) interim response so an
	// optimistic client that is streaming the whole body can resume after an
	// interruption. Browsers do not surface 1xx to JS; this is only useful to
	// library/curl clients. We set only Location here and clear it again so it
	// is not duplicated in (or does not pollute) the final response decision.
	if r.ContentLength != 0 {
		w.Header().Set("Location", u.location())
		w.WriteHeader(104)
		w.Header().Del("Location")
	}

	// Stream whatever body was included into the partial file starting at 0.
	u.mu.Lock()
	defer u.mu.Unlock()

	n, serverErr, copyErr := writePartialAt(u, 0, r.Body)
	u.offset = n
	u.updatedAt = time.Now()

	// Surface a server-side storage failure immediately rather than returning a
	// 201 that hides it (the upload would then fail on the first PATCH and the
	// browser would hang). Drain the body so the request completes cleanly.
	if serverErr != nil {
		io.Copy(io.Discard, r.Body)
		os.Remove(u.partialPath())
		registry.remove(u.id)
		u.deleted = true
		uploadsError.Add(1)
		writeProblem(w, http.StatusInternalServerError, "", "unable to store upload data")
		return
	}

	if u.lengthKnown && u.offset > u.length {
		os.Remove(u.partialPath())
		registry.remove(u.id)
		u.deleted = true
		uploadsError.Add(1)
		writeProblem(w, http.StatusBadRequest, problemInconsistentLen, "received more data than declared length")
		return
	}

	// If the client said this is the whole representation and we received all
	// of it without error, finalize now.
	if complete && copyErr == nil {
		if err := finalizeUpload(u); err != nil {
			uploadsError.Add(1)
			writeProblem(w, http.StatusInternalServerError, "", "unable to finalize upload: "+err.Error())
			return
		}
		uploadsSuccess.Add(1)
		resumableCompleted.Add(1)
		setUploadStateHeaders(w, u)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "File uploaded successfully to %s\n", "/"+u.targetPath)
		return
	}

	// Otherwise the upload stays open. Point the client at the upload resource
	// so it can append the remaining data (spec §4.2.2).
	uploadsSuccess.Add(1)
	w.Header().Set("Location", u.location())
	setUploadStateHeaders(w, u)
	w.WriteHeader(http.StatusCreated)
}

// creationLength derives the declared upload length from the request, per
// spec §4.1.3. Returns (length, known, ok). ok is false (and a problem already
// written) on an inconsistent declaration.
func creationLength(w http.ResponseWriter, r *http.Request, complete bool) (int64, bool, bool) {
	var fromContent int64 = -1
	if complete && r.ContentLength >= 0 {
		fromContent = r.ContentLength
	}

	if h := r.Header.Get("Upload-Length"); h != "" {
		ul, ok := parseSFInteger(h)
		if !ok {
			writeProblem(w, http.StatusBadRequest, "", "invalid Upload-Length header")
			return 0, false, false
		}
		if fromContent >= 0 && fromContent != ul {
			writeProblem(w, http.StatusBadRequest, problemInconsistentLen, "Upload-Length and Content-Length disagree")
			return 0, false, false
		}
		return ul, true, true
	}

	if fromContent >= 0 {
		return fromContent, true, true
	}
	return 0, false, true
}

// ---- Offset retrieval (HEAD) ----------------------------------------------

func headUpload(w http.ResponseWriter, r *http.Request, id string) {
	u, ok := registry.get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.deleted {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	setUploadStateHeaders(w, u)
	w.WriteHeader(http.StatusNoContent)
}

// ---- Append (PATCH) --------------------------------------------------------

func appendToUpload(w http.ResponseWriter, r *http.Request, id string) {
	uploadsTotal.Add(1)
	resumableAppends.Add(1)

	if mt := mediaType(r.Header.Get("Content-Type")); mt != partialUploadMediaType {
		uploadsError.Add(1)
		writeProblem(w, http.StatusUnsupportedMediaType, "", "Content-Type must be "+partialUploadMediaType)
		return
	}

	complete, ok := parseSFBool(r.Header.Get("Upload-Complete"))
	if !ok {
		uploadsError.Add(1)
		writeProblem(w, http.StatusBadRequest, "", "invalid or missing Upload-Complete header")
		return
	}

	reqOffset, ok := parseSFInteger(r.Header.Get("Upload-Offset"))
	if !ok {
		uploadsError.Add(1)
		writeProblem(w, http.StatusBadRequest, "", "invalid or missing Upload-Offset header")
		return
	}

	u, ok := registry.get(id)
	if !ok {
		uploadsError.Add(1)
		http.NotFound(w, r)
		return
	}

	// No parallel transfers to the same resource (spec §4.6). Reject rather
	// than block, since the client is not permitted to upload in parallel.
	if !u.mu.TryLock() {
		uploadsError.Add(1)
		writeProblem(w, http.StatusConflict, "", "another transfer is in progress for this upload")
		return
	}
	defer u.mu.Unlock()

	if u.deleted {
		uploadsError.Add(1)
		http.NotFound(w, r)
		return
	}

	// Reject appends to an already-completed upload (spec §4.4.2).
	if u.complete {
		uploadsError.Add(1)
		w.Header().Set("Location", "/"+u.targetPath)
		setUploadStateHeaders(w, u)
		writeProblem(w, http.StatusConflict, problemCompletedUpload, "upload is already complete")
		return
	}

	if reqOffset != u.offset {
		uploadsError.Add(1)
		setUploadStateHeaders(w, u) // echoes the correct Upload-Offset
		writeProblem(w, http.StatusConflict, problemMismatchingOffset, "Upload-Offset does not match the current offset")
		return
	}

	n, serverErr, copyErr := writePartialAt(u, u.offset, r.Body)
	u.offset += n
	u.updatedAt = time.Now()

	// A server-side I/O error (e.g. the uploads dir is missing or not writable)
	// is not something the client can resume from. Drain the rest of the body
	// so the request completes instead of leaving the browser hung on a
	// half-read upload, then report a hard failure.
	if serverErr != nil {
		io.Copy(io.Discard, r.Body)
		uploadsError.Add(1)
		writeProblem(w, http.StatusInternalServerError, "", "unable to store upload data")
		return
	}

	if u.lengthKnown && u.offset > u.length {
		os.Remove(u.partialPath())
		registry.remove(u.id)
		u.deleted = true
		uploadsError.Add(1)
		writeProblem(w, http.StatusBadRequest, problemInconsistentLen, "received more data than declared length")
		return
	}

	// The transfer was interrupted; we kept what arrived. Stay incomplete and
	// let the client resume from the new offset.
	if copyErr != nil {
		uploadsError.Add(1)
		setUploadStateHeaders(w, u)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if complete {
		if u.lengthKnown && u.offset != u.length {
			uploadsError.Add(1)
			writeProblem(w, http.StatusBadRequest, problemInconsistentLen, "completing below declared length")
			return
		}
		if err := finalizeUpload(u); err != nil {
			uploadsError.Add(1)
			writeProblem(w, http.StatusInternalServerError, "", "unable to finalize upload: "+err.Error())
			return
		}
		uploadsSuccess.Add(1)
		resumableCompleted.Add(1)
		setUploadStateHeaders(w, u)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "File uploaded successfully to %s\n", "/"+u.targetPath)
		return
	}

	uploadsSuccess.Add(1)
	setUploadStateHeaders(w, u)
	w.WriteHeader(http.StatusNoContent)
}

// ---- Cancellation (DELETE) -------------------------------------------------

func cancelUpload(w http.ResponseWriter, r *http.Request, id string) {
	u, ok := registry.get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Remove from the registry first, then take the per-upload lock (which
	// waits for any in-flight append to finish) before deleting the partial
	// file. This avoids racing an append that is mid-copy.
	registry.remove(id)
	u.mu.Lock()
	u.deleted = true
	os.Remove(u.partialPath())
	u.mu.Unlock()
	resumableCanceled.Add(1)
	w.WriteHeader(http.StatusNoContent)
}

// ---- OPTIONS ---------------------------------------------------------------

func optionsUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Accept-Patch", partialUploadMediaType)
	w.Header().Set("Allow", "HEAD, PATCH, DELETE, OPTIONS")
	w.WriteHeader(http.StatusNoContent)
}

// handleOptions answers an OPTIONS request to a target (file) URI, advertising
// support for resumable uploads (spec §4.1.4).
func handleOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Accept-Patch", partialUploadMediaType)
	w.Header().Set("Allow", "GET, HEAD, PUT, POST, OPTIONS")
	w.WriteHeader(http.StatusNoContent)
}

// ---- Helpers ---------------------------------------------------------------

// resolveTarget validates a request URL path as an upload target and returns
// the relative target path (e.g. "sub/file.txt") and absolute full path. It
// applies the same containment and trailing-slash rules as the legacy upload.
func resolveTarget(w http.ResponseWriter, urlPath string) (string, string, bool) {
	fullPath := filepath.Join(filesDir, urlPath)
	absFilesDir, _ := filepath.Abs(filesDir)
	absPath, _ := filepath.Abs(fullPath)
	if absPath != absFilesDir && !strings.HasPrefix(absPath, absFilesDir+string(os.PathSeparator)) {
		writeProblem(w, http.StatusForbidden, "", "invalid file path")
		return "", "", false
	}
	if strings.HasSuffix(urlPath, "/") {
		writeProblem(w, http.StatusBadRequest, "", "cannot upload to a directory path")
		return "", "", false
	}
	target := strings.TrimPrefix(urlPath, "/")
	return target, fullPath, true
}

// writePartialAt writes src into the upload's partial file starting at off and
// returns the number of bytes written. A short/interrupted read still returns
// the bytes that were durably written so the offset can advance.
//
// It distinguishes two failure classes. serverErr is an I/O error owned by the
// server (the partial file cannot be opened, sought, or synced) — typically a
// missing or non-writable uploads dir; this is fatal for the upload and must
// not be presented to the client as a resumable interruption. clientErr is a
// read error from src (the transfer was interrupted) and is recoverable: the
// durably written bytes still count and the client can resume.
func writePartialAt(u *upload, off int64, src io.Reader) (n int64, serverErr, clientErr error) {
	f, err := os.OpenFile(u.partialPath(), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err, nil
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, err, nil
	}
	n, clientErr = io.Copy(f, src)
	if err := f.Sync(); err != nil {
		// A sync failure means the bytes are not durable: treat it as a server
		// error so we do not advance the offset over data that may be lost.
		return n, err, nil
	}
	return n, nil, clientErr
}

// finalizeUpload marks the upload complete and moves the assembled partial file
// to its target path under filesDir. The caller must hold u.mu.
func finalizeUpload(u *upload) error {
	_, fullPath, ok := validateStoredTarget(u.targetPath)
	if !ok {
		return errors.New("target path is no longer valid")
	}
	if err := os.MkdirAll(filepath.Dir(fullPath), os.ModePerm); err != nil {
		return err
	}
	if err := moveFile(u.partialPath(), fullPath); err != nil {
		return err
	}
	invalidateDirSizes(fullPath)
	u.complete = true
	if !u.lengthKnown {
		u.length = u.offset
		u.lengthKnown = true
	}
	u.updatedAt = time.Now()
	return nil
}

// validateStoredTarget re-validates a stored target path at finalize time.
func validateStoredTarget(target string) (string, string, bool) {
	fullPath := filepath.Join(filesDir, target)
	absFilesDir, _ := filepath.Abs(filesDir)
	absPath, _ := filepath.Abs(fullPath)
	if absPath != absFilesDir && !strings.HasPrefix(absPath, absFilesDir+string(os.PathSeparator)) {
		return "", "", false
	}
	return target, fullPath, true
}

// moveFile renames src to dst, falling back to copy+remove across filesystems
// (os.Rename fails with EXDEV when src and dst are on different mounts).
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !isCrossDevice(err) {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	buf := make([]byte, moveBufSize)
	if _, err := io.CopyBuffer(out, in, buf); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	os.Remove(src)
	return nil
}

func isCrossDevice(err error) bool {
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return errors.Is(linkErr.Err, syscall.EXDEV)
	}
	return errors.Is(err, syscall.EXDEV)
}

// setUploadStateHeaders writes the Upload-Offset, Upload-Complete and (when
// known) Upload-Length headers describing the upload's current state.
func setUploadStateHeaders(w http.ResponseWriter, u *upload) {
	w.Header().Set("Upload-Offset", strconv.FormatInt(u.offset, 10))
	w.Header().Set("Upload-Complete", formatSFBool(u.complete))
	if u.lengthKnown {
		w.Header().Set("Upload-Length", strconv.FormatInt(u.length, 10))
	}
}

// mediaType returns the bare media type from a Content-Type header value,
// dropping any parameters and surrounding whitespace.
func mediaType(v string) string {
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// ---- Structured Fields (minimal subset of RFC 8941) ------------------------

// parseSFBool parses a Structured Fields Boolean. It accepts only the exact
// tokens "?1" (true) and "?0" (false).
func parseSFBool(v string) (bool, bool) {
	switch strings.TrimSpace(v) {
	case "?1":
		return true, true
	case "?0":
		return false, true
	default:
		return false, false
	}
}

func formatSFBool(b bool) string {
	if b {
		return "?1"
	}
	return "?0"
}

// parseSFInteger parses a non-negative Structured Fields Integer. It rejects
// a leading '+', signs, decimals and empty values.
func parseSFInteger(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// ---- Problem responses (RFC 9457) -----------------------------------------

// writeProblem writes an application/problem+json error response. typeURN may
// be empty for a generic problem.
func writeProblem(w http.ResponseWriter, status int, typeURN, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	body := map[string]any{
		"title":  http.StatusText(status),
		"status": status,
		"detail": detail,
	}
	if typeURN != "" {
		body["type"] = typeURN
	}
	json.NewEncoder(w).Encode(body)
}
