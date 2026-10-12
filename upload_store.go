package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Sidecars live beside the partial files, on the same persistent uploads volume.
// Each replacement is atomic; data is synced before its offset is advertised.
type storedUpload struct {
	Version     int       `json:"version"`
	ID          string    `json:"id"`
	Target      string    `json:"target"`
	SHA256      string    `json:"sha256,omitempty"`
	Offset      int64     `json:"offset"`
	Length      int64     `json:"length"`
	LengthKnown bool      `json:"lengthKnown"`
	Complete    bool      `json:"complete"`
	Temporary   bool      `json:"temporary"`
	ExpiresAt   int64     `json:"expiresAt,omitempty"`
	Deleted     bool      `json:"deleted,omitempty"`
	Finalizing  bool      `json:"finalizing,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func (u *upload) metadataPath() string { return u.partialPath() + ".json" }

// Caller holds u.mu.
func (u *upload) persist() error {
	data, err := json.Marshal(storedUpload{
		Version: 1, ID: u.id, Target: u.targetPath, SHA256: u.sha256,
		Offset: u.offset, Length: u.length, LengthKnown: u.lengthKnown,
		Complete: u.complete, Temporary: u.temporary, ExpiresAt: u.expiresAt,
		Deleted: u.deleted, Finalizing: u.finalizing, CreatedAt: u.createdAt, UpdatedAt: u.updatedAt,
	})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(uploadsDir, ".state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), u.metadataPath()); err != nil {
		return err
	}
	return syncUploadsDir()
}

func syncUploadsDir() error {
	f, err := os.Open(uploadsDir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func removeUploadFile(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Save a deletion marker before removing files so a crash during cleanup cannot
// resurrect a canceled resource. Caller holds u.mu.
func (u *upload) discard(r *uploadRegistry) error {
	u.deleted = true
	markerErr := u.persist()
	r.remove(u.id)
	dataErr := removeUploadFile(u.partialPath())
	metaErr := removeUploadFile(u.metadataPath())
	if metaErr == nil {
		markerErr = nil // With no metadata remaining there is nothing to restore.
	}
	return errors.Join(markerErr, dataErr, metaErr, syncUploadsDir())
}

func loadUploads(r *uploadRegistry) error {
	if err := os.MkdirAll(uploadsDir, 0700); err != nil {
		return err
	}
	probe, err := os.CreateTemp(uploadsDir, ".write-probe-")
	if err != nil {
		return fmt.Errorf("uploads directory is not writable: %w", err)
	}
	probe.Close()
	os.Remove(probe.Name())
	root, err := os.OpenRoot(uploadsDir)
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := os.ReadDir(uploadsDir)
	if err != nil {
		return err
	}
	loaded := make(map[string]*upload)
	for _, entry := range entries {
		name := entry.Name()
		id := strings.TrimSuffix(name, ".json")
		if name == id || !uploadIDPattern.MatchString(id) {
			continue
		}
		u, err := restoreUpload(root, name, id)
		if err != nil {
			log.Printf("ignoring upload metadata %s: %v", name, err)
		}
		if u == nil {
			if err := errors.Join(removeUploadFile(filepath.Join(uploadsDir, name)), removeUploadFile(filepath.Join(uploadsDir, id))); err != nil {
				return err
			}
			continue
		}
		if err := u.persist(); err != nil {
			return err
		}
		loaded[id] = u
	}
	// Untracked partials from older versions, and interrupted metadata writes,
	// have no usable identity. Leave unrelated files and directories alone.
	for _, entry := range entries {
		name := entry.Name()
		if (uploadIDPattern.MatchString(name) && loaded[name] == nil) || strings.HasPrefix(name, ".state-") {
			if err := removeUploadFile(filepath.Join(uploadsDir, name)); err != nil {
				return err
			}
		}
	}
	r.mu.Lock()
	r.m = loaded
	r.mu.Unlock()
	return syncUploadsDir()
}

func restoreUpload(root *os.Root, name, id string) (*upload, error) {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64*1024 {
		return nil, errors.New("invalid metadata file")
	}
	data, err := root.ReadFile(name)
	if err != nil {
		return nil, err
	}
	var s storedUpload
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	if s.Version != 1 || s.ID != id || s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() ||
		s.Offset < 0 || s.Length < 0 || (s.LengthKnown && s.Offset > s.Length) ||
		(s.Complete && (!s.LengthKnown || s.Offset != s.Length)) ||
		(s.SHA256 != "" && (!uploadHashPattern.MatchString(s.SHA256) || !s.LengthKnown)) {
		return nil, errors.New("invalid upload state")
	}
	if s.Deleted || time.Since(s.UpdatedAt) >= uploadMaxAge {
		return nil, nil
	}
	target, err := userPath("/" + s.Target)
	if err != nil || target != s.Target || target == "." {
		return nil, errors.New("invalid upload target")
	}
	u := &upload{
		id: id, targetPath: target, sha256: s.SHA256, offset: s.Offset,
		length: s.Length, lengthKnown: s.LengthKnown, complete: s.Complete,
		temporary: s.Temporary, expiresAt: s.ExpiresAt, createdAt: s.CreatedAt, updatedAt: s.UpdatedAt,
	}
	if u.complete {
		return u, nil
	}
	info, err = root.Lstat(id)
	if errors.Is(err, os.ErrNotExist) && s.Finalizing && s.SHA256 != "" {
		// Installation removes the partial only after the final file is in place.
		// Recover an interrupted completion without resetting its expiry timer.
		return recoverInstalledUpload(u)
	}
	if err != nil || !info.Mode().IsRegular() || (u.lengthKnown && info.Size() > u.length) {
		return nil, errors.New("missing or invalid partial file")
	}
	// A process can stop halfway through a PATCH, before the next sidecar write.
	// The stored bytes are authoritative; the final digest checks their content.
	u.offset = info.Size()
	return u, nil
}

func recoverInstalledUpload(u *upload) (*upload, error) {
	root, err := os.OpenRoot(filesDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(u.targetPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != u.length {
		return nil, errors.New("installed file does not match upload")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	if hex.EncodeToString(h.Sum(nil)) != u.sha256 {
		return nil, errors.New("installed file does not match upload hash")
	}
	u.offset, u.complete = u.length, true
	fileActions.Lock()
	defer fileActions.Unlock()
	if expiry, ok := fileActions.expirations[u.targetPath]; ok {
		u.expiresAt = expiry.ExpiresAt.Unix()
	}
	return u, nil
}
