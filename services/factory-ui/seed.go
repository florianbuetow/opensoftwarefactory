package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// seedDefaults stores every regular file below dir at its relative object ID,
// retrying while the blobstore starts. Missing and empty directories are valid.
func seedDefaults(ctx context.Context, blobs, dir string, logger *slog.Logger) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := seedDirectory(ctx, blobs, dir)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			logger.Error("seeding factory objects failed", "directory", dir, "error", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// seedDirectory stores every regular file below dir without overwriting an
// existing object. Object IDs always use slash-separated paths.
func seedDirectory(ctx context.Context, blobs, dir string) (err error) {
	root, err := os.OpenRoot(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = closeAfter(err, root) }()
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		id, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		body, readErr := root.ReadFile(id)
		if readErr != nil {
			return readErr
		}
		return ensureObject(ctx, blobs, filepath.ToSlash(id), string(body))
	})
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ensureObject PUTs an object unless it already exists.
func ensureObject(ctx context.Context, blobs, id, body string) error {
	base := strings.TrimRight(blobs, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/meta/"+id, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	err = resp.Body.Close()
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode != http.StatusNotFound:
		return fmt.Errorf("GET meta %s: %s", id, resp.Status)
	}
	put, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/blobs/"+id, strings.NewReader(body))
	if err != nil {
		return err
	}
	put.Header.Set("X-Creator", "factory")
	resp, err = http.DefaultClient.Do(put)
	if err != nil {
		return err
	}
	err = resp.Body.Close()
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("PUT %s: %s", id, resp.Status)
	}
	return nil
}
