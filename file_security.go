package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

var browserOriginProtection = http.NewCrossOriginProtection()

// Reject aliases as well as traversal: symlinks inside the files tree could
// otherwise expose the private expiry store under an ordinary URL.
func checkUserPath(root *os.Root, path string) error {
	if path == "." {
		return nil
	}
	if !filepath.IsLocal(path) || reservedFilePath(path) || strings.Contains(path, "\\") {
		return errors.New("invalid file path")
	}
	parts := strings.Split(filepath.ToSlash(path), "/")
	for _, part := range parts {
		if part == "." || part == ".." || part == "" {
			return errors.New("invalid file path")
		}
	}
	current := ""
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symbolic links are not accessible")
		}
	}
	return nil
}

func userPath(urlPath string) (string, error) {
	rel := strings.TrimSuffix(strings.TrimPrefix(urlPath, "/"), "/")
	if rel == "" {
		rel = "."
	}
	root, err := os.OpenRoot(filesDir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := checkUserPath(root, rel); err != nil {
		return "", err
	}
	return rel, nil
}

// Keep internal state in a dedicated root; reject a pre-existing symlink.
func openMetadataRoot(create bool) (*os.Root, error) {
	root, err := os.OpenRoot(filesDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if create {
		if err := root.Mkdir(metadataDir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	info, err := root.Lstat(metadataDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("invalid metadata directory")
	}
	return root.OpenRoot(metadataDir)
}

// Receive bytes without holding the file-actions lock. Install a complete file
// atomically under filesDir, using a private temporary file on the same volume.
func installUploadedFile(src, target string) error {
	root, err := os.OpenRoot(filesDir)
	if err != nil {
		return err
	}
	defer root.Close()
	if target == "." {
		return errors.New("invalid upload target")
	}
	if err := checkUserPath(root, target); err != nil {
		return err
	}
	if err := root.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	meta, err := openMetadataRoot(true)
	if err != nil {
		return err
	}
	defer meta.Close()
	id, err := newUploadID()
	if err != nil {
		return err
	}
	tmp := "upload-" + id
	out, err := meta.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer meta.Remove(tmp)
	in, err := os.Open(src)
	if err != nil {
		out.Close()
		return err
	}
	defer in.Close()
	_, copyErr := io.CopyBuffer(out, in, make([]byte, moveBufSize))
	if copyErr == nil {
		copyErr = out.Sync()
	}
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := root.Rename(filepath.Join(metadataDir, tmp), target); err != nil {
		return err
	}
	// The installed file is authoritative even if partial cleanup fails.
	os.Remove(src)
	return nil
}
