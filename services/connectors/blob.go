package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var errNotFound = errors.New("not found")

// blobClient reads and writes objects of the blob store over HTTP.
type blobClient struct {
	http *http.Client
	base string
}

// closeAfter closes c and adds a failed Close to the error the caller returns.
func closeAfter(err error, c io.Closer) error {
	if closeErr := c.Close(); closeErr != nil {
		return errors.Join(err, closeErr)
	}
	return err
}

func (b blobClient) get(ctx context.Context, id string) ([]byte, error) {
	data, _, err := b.getWithRevision(ctx, id)
	return data, err
}

func (b blobClient) getWithRevision(ctx context.Context, id string) (data []byte, revision string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.base+"/blobs/"+id, http.NoBody)
	if err != nil {
		return nil, "", err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("the blob store is not reachable: %w", err)
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", errNotFound
	}
	if resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("reading %s from the blob store: %s", id, resp.Status)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, "", err
	}
	return data, strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

var errPrecondition = errors.New("precondition failed")

func (b blobClient) putConditionalJSON(ctx context.Context, id string, v any, revision string, create bool) (stored string, err error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, b.base+"/blobs/"+id, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Creator", "connectors")
	if create {
		req.Header.Set("If-None-Match", "*")
	} else {
		req.Header.Set("If-Match", `"`+revision+`"`)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("the blob store is not reachable: %w", err)
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode == http.StatusPreconditionFailed {
		return "", errPrecondition
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("writing %s to the blob store: %s", id, resp.Status)
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

func (b blobClient) getJSON(ctx context.Context, id string, out any) error {
	data, err := b.get(ctx, id)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (b blobClient) put(ctx context.Context, id string, data []byte, contentType, creator, trace string) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, b.base+"/blobs/"+id, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Creator", creator)
	if trace != "" {
		req.Header.Set("X-Trace-Id", trace)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("the blob store is not reachable: %w", err)
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("writing %s to the blob store: %s", id, resp.Status)
	}
	return nil
}

func (b blobClient) putJSON(ctx context.Context, id string, v any, creator, trace string) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return b.put(ctx, id, data, "application/json", creator, trace)
}

func (b blobClient) delete(ctx context.Context, id string) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, b.base+"/blobs/"+id, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("the blob store is not reachable: %w", err)
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("deleting %s from the blob store: %s", id, resp.Status)
	}
	return nil
}

// list returns the IDs of the objects directly under a prefix; a prefix that does not exist
// has none.
func (b blobClient) list(ctx context.Context, prefix string) (ids []string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.base+"/list/"+prefix, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the blob store is not reachable: %w", err)
	}
	defer func() { err = closeAfter(err, resp.Body) }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("listing %s in the blob store: %s", prefix, resp.Status)
	}
	var listing struct {
		Objects []struct {
			ID string `json:"id"`
		} `json:"objects"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&listing)
	if err != nil {
		return nil, err
	}
	ids = make([]string, 0, len(listing.Objects))
	for _, o := range listing.Objects {
		ids = append(ids, o.ID)
	}
	return ids, nil
}
