package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Meta is the metadata the blobstore records for every object.
type Meta struct {
	Creator               string
	TraceID               string
	EstimatedInputTokens  *int64
	EstimatedOutputTokens *int64
	ProcessingTimeMS      *int64
	EstimationMethod      string
}

// Store keeps artifacts addressed by object ID.
type Store interface {
	Put(ctx context.Context, id string, data []byte, meta Meta) error
	Get(ctx context.Context, id string) ([]byte, error)
}

type PrefixStore interface {
	Store
	List(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, id string) error
}

type TreeStore interface {
	PrefixStore
	ListChildren(ctx context.Context, prefix string) (directories, objects []string, err error)
}

var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*$`)

// validID reports whether id is an object ID the blobstore service accepts.
func validID(id string) bool {
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

func (s HTTPStore) do(ctx context.Context, method, id string, body []byte, meta Meta) ([]byte, error) {
	if !validID(id) {
		return nil, fmt.Errorf("invalid object id %q", id)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.BaseURL, "/")+"/"+id, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if meta.Creator != "" {
		req.Header.Set("X-Creator", meta.Creator)
	}
	if meta.TraceID != "" {
		req.Header.Set("X-Trace-Id", meta.TraceID)
	}
	setInt64Header(req, "X-Estimated-Input-Tokens", meta.EstimatedInputTokens)
	setInt64Header(req, "X-Estimated-Output-Tokens", meta.EstimatedOutputTokens)
	setInt64Header(req, "X-Processing-Time-Ms", meta.ProcessingTimeMS)
	if meta.EstimationMethod != "" {
		req.Header.Set("X-Estimation-Method", meta.EstimationMethod)
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
		return nil, notFoundError{fmt.Sprintf("%s %s: %s", method, id, resp.Status)}
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s %s: %s", method, id, resp.Status)
	}
	return data, nil
}

// notFoundError marks a missing object; its text is the same as for any other status.
type notFoundError struct{ text string }

func (e notFoundError) Error() string { return e.text }

func setInt64Header(req *http.Request, name string, value *int64) {
	if value != nil {
		req.Header.Set(name, strconv.FormatInt(*value, 10))
	}
}

func (s HTTPStore) Put(ctx context.Context, id string, data []byte, meta Meta) error {
	_, err := s.do(ctx, http.MethodPut, id, data, meta)
	return err
}

func (s HTTPStore) Get(ctx context.Context, id string) ([]byte, error) {
	return s.do(ctx, http.MethodGet, id, nil, Meta{})
}

func (s HTTPStore) Delete(ctx context.Context, id string) error {
	_, err := s.do(ctx, http.MethodDelete, id, nil, Meta{})
	return err
}

func (s HTTPStore) List(ctx context.Context, prefix string) ([]string, error) {
	_, objects, err := s.ListChildren(ctx, prefix)
	return objects, err
}

func (s HTTPStore) ListChildren(ctx context.Context, prefix string) ([]string, []string, error) {
	if !validID(prefix) {
		return nil, nil, fmt.Errorf("invalid prefix %q", prefix)
	}
	root := strings.TrimSuffix(strings.TrimRight(s.BaseURL, "/"), "/blobs")
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
		return []string{}, []string{}, nil
	}
	if resp.StatusCode/100 != 2 {
		return nil, nil, fmt.Errorf("LIST %s: %s", prefix, resp.Status)
	}
	var listing struct {
		Directories []string `json:"directories"`
		Objects     []struct {
			ID string `json:"id"`
		} `json:"objects"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&listing); err != nil {
		return nil, nil, err
	}
	ids := make([]string, 0, len(listing.Objects))
	for _, object := range listing.Objects {
		ids = append(ids, object.ID)
	}
	dirs := make([]string, 0, len(listing.Directories))
	for _, dir := range listing.Directories {
		dirs = append(dirs, prefix+"/"+dir)
	}
	return dirs, ids, nil
}
