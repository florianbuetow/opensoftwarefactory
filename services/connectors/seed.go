package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

type repositorySeeds struct {
	Repositories []repository `json:"repositories"`
}

// readRepositorySeeds reads optional repository records mounted outside the service image.
// A missing file means the service starts with no seed data.
func readRepositorySeeds(path string) ([]repository, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var raw repositorySeeds
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&raw); err != nil {
		return nil, err
	}
	if err = d.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("seed file must contain one JSON object")
	}
	seen := map[string]bool{}
	for i, source := range raw.Repositories {
		if !repositoryName.MatchString(source.Name) {
			return nil, fmt.Errorf("repository %d has an invalid name %q", i+1, source.Name)
		}
		if seen[source.Name] {
			return nil, fmt.Errorf("repository %q is listed twice", source.Name)
		}
		seen[source.Name] = true
		idle, approve := source.IdleMinutes, source.ApprovePublish
		input := repositoryInput{
			Kind: source.Kind, URL: source.URL, DefaultBranch: source.DefaultBranch,
			SetupCommand: source.SetupCommand, IdleMinutes: &idle, ApprovePublish: &approve, Credential: source.Credential,
		}
		repo, validateErr := input.repository(source.Name, false)
		if validateErr != nil {
			return nil, fmt.Errorf("repository %q: %w", source.Name, validateErr)
		}
		if repo.Credential != "none" {
			return nil, fmt.Errorf("repository %q: a seed cannot contain a credential", source.Name)
		}
		raw.Repositories[i] = repo
	}
	return raw.Repositories, nil
}

// seedUntilDone adds externally supplied repositories, and keeps trying until the blob store answers.
func (s *service) seedUntilDone(ctx context.Context, repositories []repository) {
	for {
		err := s.seedRepositories(ctx, repositories)
		if err == nil {
			return
		}
		s.log.Warn("adding the demo repositories failed; trying again", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// seedRepositories stores externally supplied records that have no entry yet. Existing entries
// are never changed, so a person's edits stay.
func (s *service) seedRepositories(ctx context.Context, repositories []repository) error {
	for _, repo := range repositories {
		_, err := s.loadRepository(ctx, repo.Name)
		if err == nil {
			continue
		}
		if !errors.Is(err, errNotFound) {
			return err
		}
		if err = s.blobs.putJSON(ctx, repositoryObject(repo.Name), repo, "connectors", ""); err != nil {
			return err
		}
		s.log.Info("added a seeded repository", "repository", repo.Name, "url", repo.URL)
	}
	return nil
}
