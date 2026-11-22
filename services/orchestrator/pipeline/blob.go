package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Meta is the metadata the blobstore records for every object.
type Meta struct {
	Creator string
	TraceID string
}

// Store keeps artifacts addressed by object ID.
type Store interface {
	Put(ctx context.Context, id string, data []byte, meta Meta) error
	Get(ctx context.Context, id string) ([]byte, error)
}

// ErrNotFound reports a missing artifact.
var ErrNotFound = errors.New("artifact not found")

// ErrPreconditionFailed reports that a conditional artifact write did not match the
// current object revision. It is deliberately separate from Store so legacy workflow
// activities and tests do not need conditional-write support.
var ErrPreconditionFailed = errors.New("artifact precondition failed")

// ConditionalStore provides the atomic create and compare-and-replace operations used by
// intake receipts. Revisions are opaque strong ETag values returned by blobstore.
type ConditionalStore interface {
	GetWithRevision(ctx context.Context, id string) (data []byte, revision string, err error)
	PutIfAbsent(ctx context.Context, id string, data []byte, meta Meta) (revision string, err error)
	PutIfMatch(ctx context.Context, id string, data []byte, meta Meta, revision string) (newRevision string, err error)
}

var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*$`)

// ValidID reports whether id is an object ID the blobstore service accepts.
func ValidID(id string) bool {
	if id == "" || len(id) > 512 {
		return false
	}
	for _, s := range strings.Split(id, "/") {
		if !segmentPattern.MatchString(s) {
			return false
		}
	}
	return true
}

// HTTPStore is the client for the blobstore service's /blobs API.
type HTTPStore struct {
	BaseURL string
	Client  *http.Client
}

// NewHTTPStoreFromEnv uses BLOB_URL, defaulting to the in-cluster blobstore.
func NewHTTPStoreFromEnv() HTTPStore {
	return HTTPStore{BaseURL: strings.TrimRight(env("BLOB_URL", "http://blobstore:8080/blobs"), "/"), Client: http.DefaultClient}
}

func (s HTTPStore) do(ctx context.Context, method, id string, body []byte, meta Meta) ([]byte, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("invalid object id %q", id)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.BaseURL+"/"+id, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if meta.Creator != "" {
		req.Header.Set("X-Creator", meta.Creator)
	}
	if meta.TraceID != "" {
		req.Header.Set("X-Trace-Id", meta.TraceID)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s %s: %s", method, id, resp.Status)
	}
	return data, nil
}

func (s HTTPStore) Put(ctx context.Context, id string, data []byte, meta Meta) error {
	_, err := s.do(ctx, http.MethodPut, id, data, meta)
	return err
}

func (s HTTPStore) putConditional(ctx context.Context, id string, data []byte, meta Meta, header, value string) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("invalid object id %q", id)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.BaseURL+"/"+id, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set(header, value)
	if meta.Creator != "" {
		req.Header.Set("X-Creator", meta.Creator)
	}
	if meta.TraceID != "" {
		req.Header.Set("X-Trace-Id", meta.TraceID)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPreconditionFailed {
		return "", ErrPreconditionFailed
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("PUT %s: %s", id, resp.Status)
	}
	etag := strings.TrimSpace(resp.Header.Get("ETag"))
	if len(etag) < 3 || etag[0] != '"' || etag[len(etag)-1] != '"' {
		return "", fmt.Errorf("PUT %s: blobstore returned no strong ETag", id)
	}
	return etag[1 : len(etag)-1], nil
}

func (s HTTPStore) PutIfAbsent(ctx context.Context, id string, data []byte, meta Meta) (string, error) {
	return s.putConditional(ctx, id, data, meta, "If-None-Match", "*")
}

func (s HTTPStore) PutIfMatch(ctx context.Context, id string, data []byte, meta Meta, revision string) (string, error) {
	if revision == "" || strings.ContainsAny(revision, "\"\r\n") {
		return "", errors.New("invalid artifact revision")
	}
	return s.putConditional(ctx, id, data, meta, "If-Match", `"`+revision+`"`)
}

func (s HTTPStore) Get(ctx context.Context, id string) ([]byte, error) {
	return s.do(ctx, http.MethodGet, id, nil, Meta{})
}

func (s HTTPStore) GetWithRevision(ctx context.Context, id string) ([]byte, string, error) {
	if !ValidID(id) {
		return nil, "", fmt.Errorf("invalid object id %q", id)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+"/"+id, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, "", readErr
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("GET %s: %s", id, resp.Status)
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if etag == "" {
		return nil, "", fmt.Errorf("GET %s: blobstore returned no strong ETag", id)
	}
	return data, etag, nil
}

// Delete removes an object; a missing object is not an error.
func (s HTTPStore) Delete(ctx context.Context, id string) error {
	_, err := s.do(ctx, http.MethodDelete, id, nil, Meta{})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// List returns the immediate child directories and object IDs of a prefix. A missing prefix
// has no children.
func (s HTTPStore) List(ctx context.Context, prefix string) (dirs []string, objects []string, err error) {
	if !ValidID(prefix) {
		return nil, nil, fmt.Errorf("invalid prefix %q", prefix)
	}
	root := strings.TrimSuffix(s.BaseURL, "/blobs")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/list/"+prefix, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil, nil
	}
	if resp.StatusCode/100 != 2 {
		return nil, nil, fmt.Errorf("GET list %s: %s", prefix, resp.Status)
	}
	var listing struct {
		Directories []string `json:"directories"`
		Objects     []struct {
			ID string `json:"id"`
		} `json:"objects"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		return nil, nil, err
	}
	for _, o := range listing.Objects {
		objects = append(objects, o.ID)
	}
	return listing.Directories, objects, nil
}

// Creator returns the creator recorded for an object, or "" when none is recorded.
func (s HTTPStore) Creator(ctx context.Context, id string) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("invalid object id %q", id)
	}
	root := strings.TrimSuffix(s.BaseURL, "/blobs")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/meta/"+id, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("GET meta %s: %s", id, resp.Status)
	}
	var meta struct {
		Creator string `json:"creator"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return "", err
	}
	return meta.Creator, nil
}

// TraceObjects lists the IDs of every object stored with the trace ID.
func (s HTTPStore) TraceObjects(ctx context.Context, traceID string) ([]string, error) {
	root := strings.TrimSuffix(s.BaseURL, "/blobs")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/trace/"+traceID, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GET trace %s: %s", traceID, resp.Status)
	}
	var listing struct {
		Objects []struct {
			ID string `json:"id"`
		} `json:"objects"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(listing.Objects))
	for _, o := range listing.Objects {
		ids = append(ids, o.ID)
	}
	return ids, nil
}

// WriteFileAtomic replaces path with data in one rename.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
