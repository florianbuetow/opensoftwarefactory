package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const configPrefix = "mcp-gateway/"

var configFiles = []string{"mcpconfig.json", "allowlist.json", "denylist.json"}

type blobStore struct {
	base   string
	client *http.Client
}

func (s blobStore) request(ctx context.Context, method, path string, data []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.base, "/")+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Creator", "mcp-gateway")
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == 404 {
		return nil, errMissing
	}
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("blobstore %s %s: HTTP %d", method, path, res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, 2<<20))
}

var errMissing = fmt.Errorf("not found")

func (s blobStore) names(ctx context.Context) ([]string, error) {
	data, err := s.request(ctx, "GET", "/list/mcp-gateway", nil)
	if err == errMissing {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var listing struct {
		Directories []string `json:"directories"`
	}
	err = json.Unmarshal(data, &listing)
	return listing.Directories, err
}

func (s blobStore) load(ctx context.Context, name string) (Config, error) {
	var c Config
	targets := []any{&c.MCPConfig, &c.Allowlist, &c.Denylist}
	for i, file := range configFiles {
		data, err := s.request(ctx, "GET", "/blobs/"+configPrefix+name+"/"+file, nil)
		if err != nil {
			return c, fmt.Errorf("%s: %w", file, err)
		}
		if err = json.Unmarshal(data, targets[i]); err != nil {
			return c, fmt.Errorf("%s: %w", file, err)
		}
	}
	c.normalize()
	return c, c.validate()
}

func (s blobStore) save(ctx context.Context, name string, c Config) error {
	for i, v := range []any{c.MCPConfig, c.Allowlist, c.Denylist} {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		if _, err = s.request(ctx, "PUT", "/blobs/"+configPrefix+name+"/"+configFiles[i], data); err != nil {
			return err
		}
	}
	return nil
}

func (s blobStore) remove(ctx context.Context, name string) error {
	for _, file := range configFiles {
		_, err := s.request(ctx, "DELETE", "/blobs/"+configPrefix+name+"/"+file, nil)
		if err != nil && err != errMissing {
			return err
		}
	}
	return nil
}
