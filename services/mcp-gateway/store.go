package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const configPrefix = "mcp-gateway/"

var configFiles = []string{"mcpconfig.json", "allowlist.json", "denylist.json"}

type blobStore struct {
	client *http.Client
	base   string
}

// closeAfter closes c and adds a close failure to err.
func closeAfter(err error, c io.Closer) error {
	if closeErr := c.Close(); closeErr != nil {
		return errors.Join(err, closeErr)
	}
	return err
}

func (s blobStore) request(ctx context.Context, method, path string, data []byte) (body []byte, err error) {
	baseURL, err := url.Parse(s.base)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL.String(), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.URL.Path = strings.TrimRight(baseURL.Path, "/") + path
	req.Header.Set("X-Creator", "mcp-gateway")
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { err = closeAfter(err, res.Body) }()
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
	err := c.validate()
	return c, err
}

func (s blobStore) save(ctx context.Context, name string, c *Config) error {
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
