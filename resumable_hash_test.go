package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func hashCreation(target, content string, temporary bool) *httptest.ResponseRecorder {
	if temporary {
		target += "?temporary=true"
	}
	return securityRequest("POST", target, strings.NewReader(""), map[string]string{
		"Upload-Complete": "?0", "Upload-Length": strconv.Itoa(len(content)),
		"Upload-SHA256": fmt.Sprintf("%x", sha256.Sum256([]byte(content))),
	})
}

func hashAppend(location, content, offset string, complete bool) *httptest.ResponseRecorder {
	return securityRequest("PATCH", location, strings.NewReader(content), map[string]string{
		"Content-Type": partialUploadMediaType, "Upload-Offset": offset, "Upload-Complete": formatSFBool(complete),
	})
}

func restartUploadRegistry(t *testing.T) {
	t.Helper()
	registry = newUploadRegistry()
	if err := loadUploads(registry); err != nil {
		t.Fatal(err)
	}
}

func TestHashDiscoveryResumesTemporaryUploadAfterRestart(t *testing.T) {
	securityWorkspace(t)
	w := hashCreation("/resume.bin", "abcdef", true)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	location := w.Header().Get("Location")
	if w = hashAppend(location, "abc", "0", false); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	restartUploadRegistry(t)
	w = hashCreation("/resume.bin", "abcdef", true)
	if w.Code != 200 || w.Header().Get("Location") != location || w.Header().Get("Upload-Offset") != "3" {
		t.Fatalf("discovery: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if len(fileActions.expirations) != 0 {
		t.Fatal("unfinished upload started expiration")
	}
	w = hashAppend(location, "def", "3", true)
	if w.Code != 201 || w.Header().Get("File-Expires-At") == "" {
		t.Fatalf("completion: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	expiry := w.Header().Get("File-Expires-At")
	data, err := os.ReadFile(filepath.Join(filesDir, "resume.bin"))
	if err != nil || string(data) != "abcdef" {
		t.Fatal(string(data), err)
	}
	restartUploadRegistry(t)
	w = securityRequest("HEAD", location, nil, nil)
	if w.Code != 204 || w.Header().Get("Upload-Complete") != "?1" || w.Header().Get("File-Expires-At") != expiry {
		t.Fatalf("completed tombstone: %d %v", w.Code, w.Header())
	}
	// Selecting a completed file again is a new upload, with a new lifetime.
	w = hashCreation("/resume.bin", "abcdef", true)
	if w.Code != 201 || w.Header().Get("Location") == location {
		t.Fatal("completed uploads were reused")
	}
}

func TestHashDiscoverySeparatesFileIdentities(t *testing.T) {
	securityWorkspace(t)
	location := hashCreation("/same.bin", "abcdef", false).Header().Get("Location")
	for _, tc := range []struct {
		path, content string
		temporary     bool
	}{
		{"/other.bin", "abcdef", false},
		{"/same.bin", "ABCDEF", false},
		{"/same.bin", "abcdef", true},
	} {
		w := hashCreation(tc.path, tc.content, tc.temporary)
		if w.Code != 201 || w.Header().Get("Location") == location {
			t.Fatalf("identity reused for %+v: %d %v", tc, w.Code, w.Header())
		}
	}
	w := securityRequest("POST", "/same.bin", strings.NewReader(""), map[string]string{
		"Upload-Complete": "?0", "Upload-Length": "7",
		"Upload-SHA256": fmt.Sprintf("%x", sha256.Sum256([]byte("abcdef"))),
	})
	if w.Code != 201 || w.Header().Get("Location") == location {
		t.Fatal("different sizes shared an upload")
	}
}

func TestConcurrentHashDiscoveryAllocatesOneResource(t *testing.T) {
	securityWorkspace(t)
	responses := make(chan *httptest.ResponseRecorder, 16)
	var wg sync.WaitGroup
	for i := 0; i < cap(responses); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses <- hashCreation("/same.bin", "abcdef", false)
		}()
	}
	wg.Wait()
	close(responses)
	var location string
	created := 0
	for w := range responses {
		if w.Code == 201 {
			created++
		} else if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if location == "" {
			location = w.Header().Get("Location")
		}
		if w.Header().Get("Location") != location {
			t.Fatal("simultaneous discovery created multiple resources")
		}
	}
	if created != 1 || len(registry.snapshot()) != 1 {
		t.Fatal("expected exactly one creation", created)
	}
}

func TestHashMismatchPreservesExistingFileAndExpiry(t *testing.T) {
	securityWorkspace(t)
	securityRequest("PUT", "/same.bin?temporary=true", strings.NewReader("original"), nil)
	originalExpiry := fileActions.expirations["same.bin"]
	location := hashCreation("/same.bin", "abcdef", false).Header().Get("Location")
	w := hashAppend(location, "ABCDEF", "0", true)
	if w.Code != 422 {
		t.Fatal(w.Code, w.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(filesDir, "same.bin"))
	if err != nil || string(data) != "original" || fileActions.expirations["same.bin"] != originalExpiry {
		t.Fatal("unverified bytes changed the existing file or its expiry")
	}
	restartUploadRegistry(t)
	if w = securityRequest("HEAD", location, nil, nil); w.Code != 404 {
		t.Fatal("invalid upload survived restart")
	}
	if w = hashCreation("/same.bin", "abcdef", false); w.Code != 201 || w.Header().Get("Location") == location {
		t.Fatal("invalid partial was rediscovered")
	}
}

func TestHashDiscoveryRequiresEmptyIncompleteCreationWithLength(t *testing.T) {
	securityWorkspace(t)
	validHash := fmt.Sprintf("%x", sha256.Sum256([]byte("abc")))
	for _, tc := range []struct{ hash, length, complete, body string }{
		{"not-a-hash", "3", "?0", ""},
		{strings.ToUpper(validHash), "3", "?0", ""},
		{validHash, "", "?0", ""},
		{validHash, "3", "?1", ""},
		{validHash, "3", "?0", "abc"},
	} {
		w := securityRequest("POST", "/bad.bin", strings.NewReader(tc.body), map[string]string{
			"Upload-SHA256": tc.hash, "Upload-Length": tc.length, "Upload-Complete": tc.complete,
		})
		if w.Code != 400 {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
	}
	if len(registry.snapshot()) != 0 {
		t.Fatal("invalid discovery created resources")
	}
}

func TestHashRecoveryIncludesBytesReceivedBeforeMetadataUpdate(t *testing.T) {
	securityWorkspace(t)
	location := hashCreation("/resume.bin", "abcdef", false).Header().Get("Location")
	hashAppend(location, "abc", "0", false)
	id := strings.TrimPrefix(location, uploadURLPrefix)
	f, err := os.OpenFile(filepath.Join(uploadsDir, id), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("d") // Simulate a stop in the next PATCH before its sidecar update.
	f.Sync()
	f.Close()
	restartUploadRegistry(t)
	w := hashCreation("/resume.bin", "abcdef", false)
	if w.Code != 200 || w.Header().Get("Upload-Offset") != "4" {
		t.Fatal(w.Code, w.Header())
	}
	if w = hashAppend(location, "ef", "4", true); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestCanceledAndExpiredHashUploadsAreNotRestored(t *testing.T) {
	securityWorkspace(t)
	canceled := hashCreation("/canceled.bin", "abc", false).Header().Get("Location")
	if w := securityRequest("DELETE", canceled, nil, nil); w.Code != 204 {
		t.Fatal(w.Code)
	}
	expired := hashCreation("/expired.bin", "abc", false).Header().Get("Location")
	u, _ := registry.get(strings.TrimPrefix(expired, uploadURLPrefix))
	u.mu.Lock()
	u.updatedAt = time.Now().Add(-uploadMaxAge - time.Minute)
	if err := u.persist(); err != nil {
		t.Fatal(err)
	}
	u.mu.Unlock()
	restartUploadRegistry(t)
	for _, location := range []string{canceled, expired} {
		if w := securityRequest("HEAD", location, nil, nil); w.Code != 404 {
			t.Fatal("removed session survived restart", w.Code)
		}
		id := strings.TrimPrefix(location, uploadURLPrefix)
		for _, name := range []string{id, id + ".json"} {
			if _, err := os.Stat(filepath.Join(uploadsDir, name)); !os.IsNotExist(err) {
				t.Fatal("removed session left data", name, err)
			}
		}
	}
}

func TestUploadJanitorSkipsLockedTransfers(t *testing.T) {
	securityWorkspace(t)
	location := hashCreation("/active.bin", "abc", false).Header().Get("Location")
	u, _ := registry.get(strings.TrimPrefix(location, uploadURLPrefix))
	u.mu.Lock()
	u.updatedAt = time.Now().Add(-uploadMaxAge - time.Minute)
	registry.expire(time.Now().Add(-uploadMaxAge))
	if u.deleted {
		t.Fatal("active transfer was expired")
	}
	u.mu.Unlock()
	registry.expire(time.Now().Add(-uploadMaxAge))
	if w := securityRequest("HEAD", location, nil, nil); w.Code != 404 {
		t.Fatal("idle expired transfer was retained")
	}
}

func TestHashRecoveryAfterInstallationBeforeCompletionStateSave(t *testing.T) {
	securityWorkspace(t)
	location := hashCreation("/resume.bin", "abc", true).Header().Get("Location")
	w := hashAppend(location, "abc", "0", true)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	expiry := w.Header().Get("File-Expires-At")
	id := strings.TrimPrefix(location, uploadURLPrefix)
	meta := filepath.Join(uploadsDir, id+".json")
	data, _ := os.ReadFile(meta)
	var state storedUpload
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	state.Complete, state.Finalizing, state.ExpiresAt = false, true, 0
	data, _ = json.Marshal(state)
	if err := os.WriteFile(meta, data, 0600); err != nil {
		t.Fatal(err)
	}
	restartUploadRegistry(t)
	w = securityRequest("HEAD", location, nil, nil)
	if w.Code != 204 || w.Header().Get("Upload-Complete") != "?1" || w.Header().Get("File-Expires-At") != expiry {
		t.Fatalf("interrupted completion: %d %v", w.Code, w.Header())
	}
}

func TestEmptyHashedUploadCompletes(t *testing.T) {
	securityWorkspace(t)
	location := hashCreation("/empty.bin", "", false).Header().Get("Location")
	w := hashAppend(location, "", "0", true)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	info, err := os.Stat(filepath.Join(filesDir, "empty.bin"))
	if err != nil || info.Size() != 0 {
		t.Fatal("empty upload was not installed")
	}
}

func TestUnhashedUploadWithUnknownLengthSurvivesRestart(t *testing.T) {
	securityWorkspace(t)
	w := securityRequest("POST", "/legacy.bin", strings.NewReader(""), map[string]string{"Upload-Complete": "?0"})
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	location := w.Header().Get("Location")
	if w = hashAppend(location, "abc", "0", false); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	restartUploadRegistry(t)
	w = securityRequest("HEAD", location, nil, nil)
	if w.Code != 204 || w.Header().Get("Upload-Offset") != "3" || w.Header().Get("Upload-Length") != "" {
		t.Fatal(w.Code, w.Header())
	}
	if w = hashAppend(location, "def", "3", true); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	restartUploadRegistry(t)
	w = securityRequest("HEAD", location, nil, nil)
	if w.Header().Get("Upload-Length") != "6" || w.Header().Get("Upload-Complete") != "?1" {
		t.Fatal(w.Header())
	}
}

func TestUploadOffsetIsNotAcknowledgedWhenMetadataCannotBeSaved(t *testing.T) {
	securityWorkspace(t)
	location := hashCreation("/resume.bin", "abcdef", false).Header().Get("Location")
	meta := filepath.Join(uploadsDir, strings.TrimPrefix(location, uploadURLPrefix)+".json")
	os.Remove(meta)
	if err := os.Mkdir(meta, 0700); err != nil {
		t.Fatal(err)
	}
	w := hashAppend(location, "abc", "0", false)
	if w.Code != 500 {
		t.Fatal("offset acknowledged without saving metadata", w.Code)
	}
	if _, err := os.Stat(filepath.Join(filesDir, "resume.bin")); !os.IsNotExist(err) {
		t.Fatal("incomplete file installed")
	}
}

func TestCorruptMetadataDoesNotPreventOtherUploadsFromResuming(t *testing.T) {
	securityWorkspace(t)
	valid := hashCreation("/valid.bin", "abc", false).Header().Get("Location")
	hashAppend(valid, "a", "0", false)
	invalid := hashCreation("/invalid.bin", "def", false).Header().Get("Location")
	id := strings.TrimPrefix(invalid, uploadURLPrefix)
	if err := os.WriteFile(filepath.Join(uploadsDir, id+".json"), []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploadsDir, "unrelated"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	restartUploadRegistry(t)
	w := hashCreation("/valid.bin", "abc", false)
	if w.Code != 200 || w.Header().Get("Location") != valid || w.Header().Get("Upload-Offset") != "1" {
		t.Fatal("valid session was lost", w.Code, w.Header())
	}
	if w = securityRequest("HEAD", invalid, nil, nil); w.Code != 404 {
		t.Fatal("corrupt session was restored")
	}
	if data, err := os.ReadFile(filepath.Join(uploadsDir, "unrelated")); err != nil || string(data) != "keep" {
		t.Fatal("unrelated uploads-directory file was removed")
	}
}

func TestUploadRecoveryRejectsSymlinkPartialsAndEscapingTargets(t *testing.T) {
	for _, attack := range []string{"symlink", "target"} {
		t.Run(attack, func(t *testing.T) {
			securityWorkspace(t)
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
				t.Fatal(err)
			}
			location := hashCreation("/resume.bin", "abc", false).Header().Get("Location")
			id := strings.TrimPrefix(location, uploadURLPrefix)
			partial := filepath.Join(uploadsDir, id)
			if attack == "symlink" {
				os.Remove(partial)
				if err := os.Symlink(outside, partial); err != nil {
					t.Skip(err)
				}
			} else {
				meta := partial + ".json"
				data, _ := os.ReadFile(meta)
				var state storedUpload
				json.Unmarshal(data, &state)
				state.Target = "../" + filepath.Base(outside)
				data, _ = json.Marshal(state)
				if err := os.WriteFile(meta, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			restartUploadRegistry(t)
			if w := securityRequest("HEAD", location, nil, nil); w.Code != 404 {
				t.Fatal("unsafe persisted session was restored")
			}
			if data, err := os.ReadFile(outside); err != nil || string(data) != "private" {
				t.Fatal("recovery modified a file outside the upload store")
			}
		})
	}
}
