package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const metadataDir = ".filebrowser"
const temporaryLifetime = 5 * time.Minute

type fileExpiry struct {
	ExpiresAt time.Time `json:"expiresAt"`
	Modified  time.Time `json:"modified"`
	Size      int64     `json:"size"`
}

// Serializes file actions, upload completion, and expiry deletion.
var fileActions = struct {
	sync.Mutex
	expirations map[string]fileExpiry
}{expirations: make(map[string]fileExpiry)}

func reservedFilePath(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == metadataDir {
			return true
		}
	}
	return false
}

func loadExpirations() error {
	root, err := openMetadataRoot(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat("expirations.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("invalid expiration state file")
	}
	f, err := root.Open("expirations.json")
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &fileActions.expirations); err != nil {
		return err
	}
	if fileActions.expirations == nil {
		fileActions.expirations = make(map[string]fileExpiry)
	}
	return nil
}

// Caller holds fileActions.Lock. The state lives on the files volume.
func saveExpirations() error {
	root, err := openMetadataRoot(true)
	if err != nil {
		return err
	}
	defer root.Close()
	data, err := json.Marshal(fileActions.expirations)
	if err != nil {
		return err
	}
	id, err := newUploadID()
	if err != nil {
		return err
	}
	tmp := "expirations-" + id
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		root.Remove(tmp)
		return err
	}
	defer root.Remove(tmp)
	return root.Rename(tmp, "expirations.json")
}

func clearFileExpiry(path string) {
	key := filepath.ToSlash(filepath.Clean(path))
	if _, ok := fileActions.expirations[key]; !ok {
		return
	}
	delete(fileActions.expirations, key)
	if err := saveExpirations(); err != nil {
		log.Printf("Saving file expirations: %v", err)
	}
}

func expiryUnix(path string) int64 {
	rel, err := filepath.Rel(filesDir, path)
	if err != nil {
		return 0
	}
	fileActions.Lock()
	defer fileActions.Unlock()
	if expiry, ok := fileActions.expirations[filepath.ToSlash(rel)]; ok {
		return expiry.ExpiresAt.Unix()
	}
	return 0
}

func handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	succeeded := false
	defer func() {
		if succeeded {
			httpRequestsSuccess.Add(1)
		} else {
			httpRequestsError.Add(1)
		}
	}()
	if !enableUpload {
		http.Error(w, "Folder creation is disabled", http.StatusForbidden)
		return
	}
	rel, err := userPath(r.URL.Path)
	if err != nil || rel == "." {
		http.Error(w, "Invalid folder name", http.StatusBadRequest)
		return
	}
	fileActions.Lock()
	defer fileActions.Unlock()
	root, err := os.OpenRoot(filesDir)
	if err != nil {
		http.Error(w, "Unable to access files", http.StatusInternalServerError)
		return
	}
	defer root.Close()
	if err := checkUserPath(root, rel); err != nil {
		http.Error(w, "Invalid file path", http.StatusForbidden)
		return
	}
	if err := root.Mkdir(rel, 0755); err != nil {
		switch {
		case errors.Is(err, os.ErrExist):
			http.Error(w, "A file or folder with that name already exists", http.StatusConflict)
		case errors.Is(err, os.ErrNotExist):
			http.Error(w, "The parent folder no longer exists", http.StatusNotFound)
		default:
			http.Error(w, "Unable to create folder", http.StatusForbidden)
		}
		return
	}
	invalidateDirSizes(filepath.Join(filesDir, rel))
	succeeded = true
	w.Header().Set("Location", r.URL.EscapedPath())
	w.WriteHeader(http.StatusCreated)
}

func handleFileAction(w http.ResponseWriter, r *http.Request) {
	succeeded := false
	defer func() {
		if succeeded {
			httpRequestsSuccess.Add(1)
		} else {
			httpRequestsError.Add(1)
		}
	}()
	if !enableUpload {
		http.Error(w, "File changes are disabled", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodPatch && r.URL.Query().Get("temporary") != "true" && r.URL.Query().Get("temporary") != "false" {
		http.Error(w, "Specify temporary=true or temporary=false", http.StatusBadRequest)
		return
	}
	_, fullPath, ok := resolveTarget(w, r.URL.Path)
	if !ok {
		return
	}
	rel, err := filepath.Rel(filesDir, fullPath)
	if err != nil || rel == "." || !filepath.IsLocal(rel) || reservedFilePath(rel) {
		http.Error(w, "Invalid file path", http.StatusForbidden)
		return
	}
	fileActions.Lock()
	defer fileActions.Unlock()
	root, err := os.OpenRoot(filesDir)
	if err != nil {
		http.Error(w, "Unable to access files", http.StatusInternalServerError)
		return
	}
	defer root.Close()
	if err := checkUserPath(root, rel); err != nil {
		http.Error(w, "Invalid file path", http.StatusForbidden)
		return
	}
	info, err := root.Lstat(rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "Unable to access file", http.StatusForbidden)
		}
		return
	}
	if !info.Mode().IsRegular() {
		http.Error(w, "Only regular files can be deleted or made temporary", http.StatusBadRequest)
		return
	}
	key := filepath.ToSlash(rel)
	if r.Method == http.MethodDelete {
		if err := root.Remove(rel); err != nil {
			http.Error(w, "Unable to delete file", http.StatusInternalServerError)
			return
		}
		clearFileExpiry(key)
		invalidateDirSizes(fullPath)
		succeeded = true
		w.WriteHeader(http.StatusNoContent)
		return
	}
	old, existed := fileActions.expirations[key]
	var expiresAt int64
	if r.URL.Query().Get("temporary") == "true" {
		expiry := fileExpiry{ExpiresAt: time.Now().Add(temporaryLifetime), Modified: info.ModTime(), Size: info.Size()}
		fileActions.expirations[key] = expiry
		expiresAt = expiry.ExpiresAt.Unix()
	} else {
		delete(fileActions.expirations, key)
	}
	if err := saveExpirations(); err != nil {
		if existed {
			fileActions.expirations[key] = old
		} else {
			delete(fileActions.expirations, key)
		}
		http.Error(w, "Unable to save file expiration", http.StatusInternalServerError)
		return
	}
	succeeded = true
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"expiresAt":%d}`, expiresAt)
}

func expireFiles() {
	fileActions.Lock()
	defer fileActions.Unlock()
	root, err := os.OpenRoot(filesDir)
	if err != nil {
		log.Printf("Expiring files: %v", err)
		return
	}
	defer root.Close()
	changed := false
	for path, expiry := range fileActions.expirations {
		if time.Now().Before(expiry.ExpiresAt) {
			continue
		}
		if path == "." || checkUserPath(root, path) != nil {
			delete(fileActions.expirations, path)
			changed = true
			continue
		}
		info, err := root.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			delete(fileActions.expirations, path)
			changed = true
			continue
		}
		if err != nil {
			log.Printf("Expiring %s: %v", path, err)
			continue
		}
		// A replacement made outside the browser should not inherit an old expiry.
		if !info.Mode().IsRegular() || !info.ModTime().Equal(expiry.Modified) || info.Size() != expiry.Size {
			delete(fileActions.expirations, path)
			changed = true
			continue
		}
		if err := root.Remove(path); err != nil {
			log.Printf("Expiring %s: %v", path, err)
			continue
		}
		invalidateDirSizes(filepath.Join(filesDir, path))
		delete(fileActions.expirations, path)
		changed = true
	}
	if changed {
		if err := saveExpirations(); err != nil {
			log.Printf("Saving file expirations: %v", err)
		}
	}
}

func startFileJanitor() {
	expireFiles()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for range ticker.C {
			expireFiles()
		}
	}()
}
