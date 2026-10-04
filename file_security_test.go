package main

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func securityWorkspace(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	for _, path := range []string{filesDir, uploadsDir} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	oldUpload, oldRegistry := enableUpload, registry
	enableUpload = true
	registry = &uploadRegistry{m: make(map[string]*upload)}
	fileActions.expirations = make(map[string]fileExpiry)
	t.Cleanup(func() {
		enableUpload = oldUpload
		registry = oldRegistry
		fileActions.expirations = make(map[string]fileExpiry)
	})
}

func securityRequest(method, path string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://files.example"+path, body)
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	pathHandler(w, r)
	return w
}

func securityWrite(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filesDir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserMutationsRejectForeignOrigins(t *testing.T) {
	securityWorkspace(t)
	securityWrite(t, "keep.txt", "keep")
	endpoints := []struct{ method, path string }{
		{"POST", "/folder/?folder=true"}, {"DELETE", "/keep.txt"},
		{"PATCH", "/keep.txt?temporary=true"}, {"PUT", "/keep.txt"},
		{"POST", "/keep.txt"}, {"DELETE", "/.uploads/" + strings.Repeat("a", 32)},
	}
	for _, endpoint := range endpoints {
		for _, headers := range []map[string]string{
			{"Sec-Fetch-Site": "cross-site"}, {"Sec-Fetch-Site": "same-site"},
			{"Origin": "https://evil.example"}, {"Origin": "null"},
		} {
			t.Run(endpoint.method+endpoint.path+headers["Sec-Fetch-Site"]+headers["Origin"], func(t *testing.T) {
				if got := securityRequest(endpoint.method, endpoint.path, strings.NewReader("bad"), headers).Code; got != http.StatusForbidden {
					t.Fatalf("status=%d, want 403", got)
				}
			})
		}
	}
	if got := securityRequest("POST", "/allowed/?folder=true", nil, map[string]string{"Origin": "http://files.example"}).Code; got != 201 {
		t.Fatalf("same-origin status=%d", got)
	}
	if got := securityRequest("POST", "/cli/?folder=true", nil, nil).Code; got != 201 {
		t.Fatalf("CLI status=%d", got)
	}
	data, err := os.ReadFile(filepath.Join(filesDir, "keep.txt"))
	if err != nil || string(data) != "keep" {
		t.Fatal("foreign-origin request modified file")
	}
}

func TestAliasesAndTraversalCannotAccessMetadataOrOutsideFiles(t *testing.T) {
	securityWorkspace(t)
	securityWrite(t, "keep.txt", "keep")
	if got := securityRequest("PATCH", "/keep.txt?temporary=true", nil, nil).Code; got != 200 {
		t.Fatal(got)
	}
	before, err := os.ReadFile(filepath.Join(filesDir, metadataDir, "expirations.json"))
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "victim.txt"), []byte("outside"), 0644); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"alias": metadataDir, "escape": outside, "linked-file": filepath.Join(metadataDir, "expirations.json")} {
		if err := os.Symlink(target, filepath.Join(filesDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	paths := []string{"/alias/expirations.json", "/escape/victim.txt", "/linked-file", "/.filebrowser/expirations.json", "/../outside.txt", "/missing/../keep.txt", "/a%5Cb.txt"}
	for _, path := range paths {
		for _, method := range []string{"GET", "PUT", "DELETE", "PATCH", "POST"} {
			t.Run(method+path, func(t *testing.T) {
				query := ""
				if method == "PATCH" {
					query = "?temporary=true"
				}
				if method == "POST" {
					query = "?folder=true"
				}
				got := securityRequest(method, path+query, strings.NewReader("corrupt"), nil).Code
				if got < 400 {
					t.Fatalf("unsafe request returned %d", got)
				}
			})
		}
	}
	after, err := os.ReadFile(filepath.Join(filesDir, metadataDir, "expirations.json"))
	if err != nil || string(after) != string(before) {
		t.Fatal("expiry state modified through an alias")
	}
	victim, err := os.ReadFile(filepath.Join(outside, "victim.txt"))
	if err != nil || string(victim) != "outside" {
		t.Fatal("outside file modified")
	}
}

func TestExpiryRejectsSymlinkedParent(t *testing.T) {
	securityWorkspace(t)
	securityWrite(t, "keep.txt", "keep")
	if err := os.Symlink(".", filepath.Join(filesDir, "alias")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(filesDir, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	fileActions.expirations["alias/keep.txt"] = fileExpiry{ExpiresAt: time.Now().Add(-time.Minute), Modified: info.ModTime(), Size: info.Size()}
	expireFiles()
	if _, err := os.Stat(filepath.Join(filesDir, "keep.txt")); err != nil {
		t.Fatal("expiry deleted file through symlink", err)
	}
}

func TestMetadataDirectoryCannotBeSymlink(t *testing.T) {
	securityWorkspace(t)
	if err := os.Mkdir(filepath.Join(filesDir, "ordinary"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("ordinary", filepath.Join(filesDir, metadataDir)); err != nil {
		t.Fatal(err)
	}
	if err := loadExpirations(); err == nil {
		t.Fatal("accepted symlinked metadata directory")
	}
	if err := saveExpirations(); err == nil {
		t.Fatal("wrote to symlinked metadata directory")
	}
}

type blockedUploadReader struct {
	started, release chan struct{}
	done             bool
}

func (r *blockedUploadReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	close(r.started)
	<-r.release
	r.done = true
	return copy(p, "replacement"), nil
}

func TestSlowUploadDoesNotBlockFileActions(t *testing.T) {
	securityWorkspace(t)
	securityWrite(t, "keep.txt", "original")
	reader := &blockedUploadReader{started: make(chan struct{}), release: make(chan struct{})}
	uploadDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { uploadDone <- securityRequest("PUT", "/keep.txt", reader, nil) }()
	released := false
	defer func() {
		if !released {
			close(reader.release)
			<-uploadDone
		}
	}()
	select {
	case <-reader.started:
	case <-time.After(2 * time.Second):
		t.Fatal("upload did not start")
	}
	listingDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { listingDone <- securityRequest("GET", "/", nil, nil) }()
	select {
	case response := <-listingDone:
		if response.Code != 200 {
			t.Fatal(response.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow upload blocked directory listing")
	}
	// Expiry changes must also stay responsive during an upload.
	actionDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { actionDone <- securityRequest("PATCH", "/keep.txt?temporary=true", nil, nil) }()
	select {
	case response := <-actionDone:
		if response.Code != 200 {
			t.Fatal(response.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slow upload blocked file actions")
	}
	before, err := os.ReadFile(filepath.Join(filesDir, "keep.txt"))
	if err != nil || string(before) != "original" {
		t.Fatal("incomplete upload replaced original")
	}
	close(reader.release)
	released = true
	if got := (<-uploadDone).Code; got != 201 {
		t.Fatal(got)
	}
	after, err := os.ReadFile(filepath.Join(filesDir, "keep.txt"))
	if err != nil || string(after) != "replacement" {
		t.Fatal("upload was not installed")
	}
	if len(fileActions.expirations) != 0 {
		t.Fatal("replacement retained old expiry")
	}
}

type brokenUploadReader struct{}

func (brokenUploadReader) Read([]byte) (int, error) { return 0, errors.New("connection failed") }

func TestInterruptedUploadPreservesExistingFile(t *testing.T) {
	securityWorkspace(t)
	securityWrite(t, "keep.txt", "original")
	if got := securityRequest("PUT", "/keep.txt", brokenUploadReader{}, nil).Code; got != 500 {
		t.Fatal(got)
	}
	data, err := os.ReadFile(filepath.Join(filesDir, "keep.txt"))
	if err != nil || string(data) != "original" {
		t.Fatal("failed upload modified original")
	}
}

func TestFilenameHeadersCannotInjectHeaders(t *testing.T) {
	for _, name := range []string{"normal.txt", "résumé 日本語.txt", "a\"; evil=\"yes.txt", "a\r\nX-Injected: yes.txt", "\\path\\file.txt", "\x00\x7f"} {
		header := fileDisposition(name)
		if strings.ContainsAny(header, "\r\n\x00\x7f") {
			t.Fatalf("unsafe header: %q", header)
		}
		disposition, params, err := mime.ParseMediaType(header)
		if err != nil || disposition != "attachment" || params["filename"] == "" {
			t.Fatalf("invalid disposition %q: %v", header, err)
		}
		if _, ok := params["evil"]; ok {
			t.Fatalf("parameter injection: %q", header)
		}
		if strings.ContainsAny(params["filename"], "/\\") {
			t.Fatalf("path in filename: %q", header)
		}
	}
}

func TestActiveContentDownloadsAndEscapedListings(t *testing.T) {
	securityWorkspace(t)
	name := "<img src=x onerror=alert(1)>.html"
	securityWrite(t, name, "<script>alert(1)</script>")
	path := "/" + url.PathEscape(name)
	for _, method := range []string{"GET", "HEAD"} {
		w := securityRequest(method, path, nil, nil)
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("unsafe response: %d %v", w.Code, w.Header())
		}
	}
	page := securityRequest("GET", "/", nil, nil).Body.String()
	if strings.Contains(page, "<img src=x onerror=alert(1)>") || !strings.Contains(page, "&lt;img") {
		t.Fatal("unescaped filename in directory listing")
	}
	ranged := securityRequest("GET", path, nil, map[string]string{"Range": "bytes=0-3"})
	if ranged.Code != 206 || ranged.Body.String() != "<scr" {
		t.Fatal("ranged download regressed")
	}
}

func TestResumableUploadRevalidatesTargetAtCompletion(t *testing.T) {
	securityWorkspace(t)
	if err := os.Mkdir(filepath.Join(filesDir, "parent"), 0755); err != nil {
		t.Fatal(err)
	}
	w := securityRequest("POST", "/parent/file.txt", strings.NewReader(""), map[string]string{"Upload-Complete": "?0", "Upload-Length": "3"})
	if w.Code != 201 {
		t.Fatal(w.Code)
	}
	outside := t.TempDir()
	if err := os.Remove(filepath.Join(filesDir, "parent")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(filesDir, "parent")); err != nil {
		t.Fatal(err)
	}
	w = securityRequest("PATCH", w.Header().Get("Location"), strings.NewReader("bad"), map[string]string{"Content-Type": partialUploadMediaType, "Upload-Complete": "?1", "Upload-Offset": "0"})
	if w.Code < 400 {
		t.Fatal("completed upload through a swapped symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "file.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("wrote outside files root")
	}
}

func TestReadOnlyModeBlocksFileMutations(t *testing.T) {
	securityWorkspace(t)
	securityWrite(t, "keep.txt", "keep")
	enableUpload = false
	for _, entry := range []struct{ method, path string }{{"POST", "/folder/?folder=true"}, {"DELETE", "/keep.txt"}, {"PATCH", "/keep.txt?temporary=true"}, {"PUT", "/keep.txt"}} {
		if got := securityRequest(entry.method, entry.path, strings.NewReader("bad"), nil).Code; got != 403 {
			t.Fatalf("%s returned %d", entry.method, got)
		}
	}
}

func TestExpiryPersistenceCancellationAndCleanup(t *testing.T) {
	securityWorkspace(t)
	securityWrite(t, "temporary.txt", "temporary")
	before := time.Now().Unix()
	if got := securityRequest("PATCH", "/temporary.txt?temporary=true", nil, nil).Code; got != 200 {
		t.Fatal(got)
	}
	fileActions.expirations = make(map[string]fileExpiry)
	if err := loadExpirations(); err != nil {
		t.Fatal(err)
	}
	expiry, ok := fileActions.expirations["temporary.txt"]
	if !ok || expiry.ExpiresAt.Unix() < before+299 || expiry.ExpiresAt.Unix() > before+301 {
		t.Fatal("five-minute expiry was not persisted")
	}
	if got := securityRequest("PATCH", "/temporary.txt?temporary=false", nil, nil).Code; got != 200 {
		t.Fatal(got)
	}
	fileActions.expirations = make(map[string]fileExpiry)
	if err := loadExpirations(); err != nil || len(fileActions.expirations) != 0 {
		t.Fatal("cancellation did not survive reload", err)
	}
	expiry.ExpiresAt = time.Now().Add(-time.Second)
	fileActions.expirations["temporary.txt"] = expiry
	if err := saveExpirations(); err != nil {
		t.Fatal(err)
	}
	fileActions.expirations = make(map[string]fileExpiry)
	if err := loadExpirations(); err != nil {
		t.Fatal(err)
	}
	expireFiles()
	if _, err := os.Stat(filepath.Join(filesDir, "temporary.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("overdue file was not removed")
	}
}

func TestResumableUploadInstallsFileAndClearsExpiry(t *testing.T) {
	securityWorkspace(t)
	securityWrite(t, "keep.txt", "original")
	if got := securityRequest("PATCH", "/keep.txt?temporary=true", nil, nil).Code; got != 200 {
		t.Fatal(got)
	}
	w := securityRequest("POST", "/keep.txt", strings.NewReader(""), map[string]string{"Upload-Complete": "?0", "Upload-Length": "3"})
	if w.Code != 201 {
		t.Fatal(w.Code)
	}
	w = securityRequest("PATCH", w.Header().Get("Location"), strings.NewReader("new"), map[string]string{"Content-Type": partialUploadMediaType, "Upload-Complete": "?1", "Upload-Offset": "0"})
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(filesDir, "keep.txt"))
	if err != nil || string(data) != "new" {
		t.Fatal("completed upload was not installed")
	}
	if len(fileActions.expirations) != 0 {
		t.Fatal("completed upload retained old expiry")
	}
}

func TestUploadDoesNotOverwriteHardlinkTarget(t *testing.T) {
	securityWorkspace(t)
	outside := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(filesDir, "linked.txt")); err != nil {
		t.Skip("hardlinks unavailable:", err)
	}
	if got := securityRequest("PUT", "/linked.txt", strings.NewReader("replacement"), nil).Code; got != 201 {
		t.Fatal(got)
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "outside" {
		t.Fatal("upload modified hardlink target")
	}
}

func TestTemporaryUploadSchedulesExpiryOnCompletion(t *testing.T) {
	securityWorkspace(t)
	w := securityRequest("POST", "/temporary.txt?temporary=true", strings.NewReader(""), map[string]string{"Upload-Complete": "?0", "Upload-Length": "3"})
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	location := w.Header().Get("Location")
	if len(fileActions.expirations) != 0 || w.Header().Get("File-Expires-At") != "" {
		t.Fatal("incomplete upload started expiry")
	}
	before := time.Now().Unix()
	w = securityRequest("PATCH", location, strings.NewReader("new"), map[string]string{"Content-Type": partialUploadMediaType, "Upload-Complete": "?1", "Upload-Offset": "0"})
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	expiry, ok := fileActions.expirations["temporary.txt"]
	if !ok || expiry.ExpiresAt.Unix() < before+299 || expiry.ExpiresAt.Unix() > before+301 {
		t.Fatal("expiry not scheduled five minutes from completion")
	}
	if w.Header().Get("File-Expires-At") == "" {
		t.Fatal("completion did not return expiry")
	}
	head := securityRequest("HEAD", location, nil, nil)
	if head.Header().Get("File-Expires-At") != w.Header().Get("File-Expires-At") {
		t.Fatal("resume lookup lost completion expiry")
	}
	fileActions.expirations = make(map[string]fileExpiry)
	if err := loadExpirations(); err != nil {
		t.Fatal(err)
	}
	if _, ok := fileActions.expirations["temporary.txt"]; !ok {
		t.Fatal("temporary upload expiry was not persisted")
	}
	if got := securityRequest("PUT", "/temporary.txt", strings.NewReader("permanent"), nil).Code; got != 201 {
		t.Fatal(got)
	}
	if len(fileActions.expirations) != 0 {
		t.Fatal("permanent replacement retained expiry")
	}
}

func TestTemporaryPutAndInvalidUploadSetting(t *testing.T) {
	securityWorkspace(t)
	w := securityRequest("PUT", "/temporary.txt?temporary=true", strings.NewReader("temporary"), nil)
	if w.Code != 201 || w.Header().Get("File-Expires-At") == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, method := range []string{"PUT", "POST"} {
		headers := map[string]string{}
		if method == "POST" {
			headers["Upload-Complete"] = "?0"
		}
		if got := securityRequest(method, "/invalid.txt?temporary=invalid", strings.NewReader("bad"), headers).Code; got != 400 {
			t.Fatal(method, got)
		}
	}
	if _, err := os.Stat(filepath.Join(filesDir, "invalid.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid setting created a file")
	}
}

func TestTemporaryUploadRequiresDurableExpiry(t *testing.T) {
	securityWorkspace(t)
	securityWrite(t, "keep.txt", "original")
	if err := os.MkdirAll(filepath.Join(filesDir, metadataDir, "expirations.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if got := securityRequest("PUT", "/keep.txt?temporary=true", strings.NewReader("replacement"), nil).Code; got < 400 {
		t.Fatal("temporary upload succeeded without persistent expiry")
	}
	data, err := os.ReadFile(filepath.Join(filesDir, "keep.txt"))
	if err != nil || string(data) != "original" {
		t.Fatal("failed temporary upload replaced original")
	}
	if len(fileActions.expirations) != 0 {
		t.Fatal("failed upload left an in-memory expiry")
	}
}
