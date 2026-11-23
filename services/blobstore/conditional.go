package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type transactionState struct {
	err error
	mu  sync.RWMutex
}

type putCondition struct {
	revision string
	kind     byte
}

const (
	conditionNone   byte = 0
	conditionCreate byte = 1
	conditionMatch  byte = 2
)

type transactionJournal struct {
	Operation     string `json:"operation"`
	ID            string `json:"id"`
	Stage         string `json:"stage,omitempty"`
	PreviousTrace string `json:"previous_trace,omitempty"`
	Meta          Meta   `json:"meta,omitempty"`
}

func parsePutCondition(r *http.Request) (putCondition, error) {
	none := r.Header.Values("If-None-Match")
	match := r.Header.Values("If-Match")
	if len(none)+len(match) == 0 {
		return putCondition{}, nil
	}
	if len(none) != 0 && len(match) != 0 {
		return putCondition{}, errors.New("combined conditions")
	}
	if len(none) != 0 {
		if len(none) != 1 || strings.TrimSpace(none[0]) != "*" {
			return putCondition{}, errors.New("malformed If-None-Match")
		}
		return putCondition{kind: conditionCreate}, nil
	}
	if len(match) != 1 {
		return putCondition{}, errors.New("multiple If-Match values")
	}
	v := strings.TrimSpace(match[0])
	if len(v) < 3 || v[0] != '"' || v[len(v)-1] != '"' || strings.HasPrefix(v, "W/") || strings.Contains(v[1:len(v)-1], `"`) || strings.Contains(v, ",") {
		return putCondition{}, errors.New("malformed If-Match")
	}
	revision := v[1 : len(v)-1]
	for _, c := range []byte(revision) {
		if c < 0x21 || c == 0x7f {
			return putCondition{}, errors.New("malformed If-Match")
		}
	}
	return putCondition{kind: conditionMatch, revision: revision}, nil
}

func quoteETag(revision string) string { return `"` + revision + `"` }

func newRevision() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func (s *store) commitPut(id string, body io.Reader, meta *Meta, condition putCondition) (revision string, status int, err error) {
	s.tx.mu.Lock()
	defer s.tx.mu.Unlock()
	if s.tx.err != nil {
		return "", 0, s.tx.err
	}
	object, _, _ := s.paths(id)
	info, statErr := os.Stat(object)
	exists := statErr == nil && !info.IsDir()
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return "", 0, statErr
	}
	current := Meta{}
	if exists {
		current = s.readMeta(id, info)
	}
	if condition.kind == conditionCreate && exists {
		return "", http.StatusPreconditionFailed, nil
	}
	if condition.kind == conditionMatch && (!exists || current.Revision == "" || current.Revision != condition.revision) {
		return "", http.StatusPreconditionFailed, nil
	}
	err = ensureDirDurable(filepath.Dir(object), s.objects)
	if err != nil {
		return "", 0, err
	}
	stage, err := os.CreateTemp(s.transactions, ".upload-*")
	if err != nil {
		return "", 0, err
	}
	stagePath := stage.Name()
	// A published stage was renamed into place and is already gone. A stage that is left
	// behind and cannot be removed fails the request, whatever its outcome was.
	defer func() {
		if cleanupErr := removeAfter(nil, stagePath); cleanupErr != nil {
			revision, status, err = "", 0, errors.Join(err, cleanupErr)
		}
	}()
	size, copyErr := copyObject(stage, body, objectSizeLimit(id))
	if copyErr == nil {
		copyErr = stage.Sync()
	}
	if closeErr := stage.Close(); copyErr == nil {
		copyErr = closeErr
	}
	var tooLarge *http.MaxBytesError
	if errors.As(copyErr, &tooLarge) {
		return "", http.StatusRequestEntityTooLarge, nil
	}
	if copyErr != nil {
		return "", 0, copyErr
	}
	err = syncDir(filepath.Dir(object))
	if err != nil {
		return "", 0, err
	}
	revision, err = newRevision()
	if err != nil {
		return "", 0, err
	}
	m := *meta
	m.Size, m.Revision = size, revision
	j := transactionJournal{Operation: "put", ID: id, Stage: stagePath, Meta: m, PreviousTrace: current.TraceID}
	journalPath := filepath.Join(s.transactions, revision+".json")
	if err = writeJSONAtomic(journalPath, &j); err == nil {
		err = s.replayTransaction(journalPath, &j)
	}
	if err != nil {
		if recoveryErr := s.recoverTransactionsLocked(); recoveryErr != nil {
			s.tx.err = fmt.Errorf("transaction failed: %v; recovery failed: %w", err, recoveryErr)
			return "", 0, s.tx.err
		}
		return "", 0, err
	}
	return revision, 0, nil
}

// objectSizeLimit preserves the established cap for ordinary blobs. Skills are
// explicitly unbounded because one immutable skill version may contain large text files.
func objectSizeLimit(id string) int64 {
	if strings.HasPrefix(id, "skills/") {
		return 0
	}
	return maxObjectBytes
}

func copyObject(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	if limit == 0 {
		return io.Copy(dst, src)
	}
	size, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if err == nil && size > limit {
		err = &http.MaxBytesError{Limit: limit}
	}
	return size, err
}

func (s *store) commitDelete(id string) (status int, err error) {
	s.tx.mu.Lock()
	defer s.tx.mu.Unlock()
	if s.tx.err != nil {
		return 0, s.tx.err
	}
	object, _, _ := s.paths(id)
	info, err := os.Stat(object)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && info.IsDir()) {
		return http.StatusNotFound, nil
	}
	if err != nil {
		return 0, err
	}
	current := s.readMeta(id, info)
	txnID, err := newRevision()
	if err != nil {
		return 0, err
	}
	j := transactionJournal{Operation: "delete", ID: id, PreviousTrace: current.TraceID}
	journalPath := filepath.Join(s.transactions, txnID+".json")
	if err = writeJSONAtomic(journalPath, &j); err == nil {
		err = s.replayTransaction(journalPath, &j)
	}
	if err != nil {
		if recoveryErr := s.recoverTransactionsLocked(); recoveryErr != nil {
			s.tx.err = fmt.Errorf("delete failed: %v; recovery failed: %w", err, recoveryErr)
			return 0, s.tx.err
		}
		return 0, err
	}
	return 0, nil
}

func (s *store) recoverTransactions() error {
	s.tx.mu.Lock()
	defer s.tx.mu.Unlock()
	return s.recoverTransactionsLocked()
}

func (s *store) recoverTransactionsLocked() error {
	entries, err := os.ReadDir(s.transactions)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.transactions, entry.Name())
		data, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return err
		}
		var j transactionJournal
		if err := json.Unmarshal(data, &j); err != nil || !validID(j.ID) || (j.Operation != "put" && j.Operation != "delete") {
			return fmt.Errorf("invalid transaction journal %s", entry.Name())
		}
		if err := s.replayTransaction(path, &j); err != nil {
			return err
		}
	}
	return nil
}

// ensureLegacyRevisions upgrades objects created before revisions existed without
// changing their bytes or established metadata. Each upgrade uses the same journal
// as a PUT, so an interrupted startup is completed by the next startup replay.
func (s *store) ensureLegacyRevisions() error {
	s.tx.mu.Lock()
	defer s.tx.mu.Unlock()
	return filepath.WalkDir(s.objects, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.objects, path)
		if err != nil {
			return err
		}
		id := filepath.ToSlash(rel)
		if !validID(id) {
			return nil
		}
		if s.isUnpublishedStage(id, entry) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		m := s.readMeta(id, info)
		if m.Revision != "" {
			return nil
		}
		revision, err := newRevision()
		if err != nil {
			return err
		}
		m.Revision = revision
		j := transactionJournal{Operation: "put", ID: id, Meta: m, PreviousTrace: m.TraceID}
		journalPath := filepath.Join(s.transactions, revision+".json")
		if err := writeJSONAtomic(journalPath, &j); err != nil {
			return err
		}
		return s.replayTransaction(journalPath, &j)
	})
}

func (s *store) replayTransaction(journalPath string, j *transactionJournal) error {
	object, metaPath, _ := s.paths(j.ID)
	switch j.Operation {
	case "put":
		if j.Stage != "" {
			stageDir := filepath.Dir(j.Stage)
			if (stageDir != filepath.Dir(object) && stageDir != s.transactions) || !strings.HasPrefix(filepath.Base(j.Stage), ".upload-") {
				return errors.New("invalid staged object path")
			}
		}
		if j.Stage != "" {
			if err := os.Rename(j.Stage, object); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		if err := syncDir(filepath.Dir(object)); err != nil {
			return err
		}
		if err := s.writeMeta(metaPath, &j.Meta); err != nil {
			return err
		}
		if j.PreviousTrace != "" && j.PreviousTrace != j.Meta.TraceID {
			s.mu.Lock()
			s.dropTraceEntry(j.PreviousTrace, j.ID)
			s.mu.Unlock()
		}
	case "delete":
		if err := os.Remove(object); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := syncNearestDir(filepath.Dir(object), s.objects); err != nil {
			return err
		}
		if err := s.removeMeta(j.ID, metaPath); err != nil {
			return err
		}
		if j.PreviousTrace != "" {
			s.mu.Lock()
			s.dropTraceEntry(j.PreviousTrace, j.ID)
			s.mu.Unlock()
		}
		pruneEmpty(filepath.Dir(object), s.objects)
		pruneEmpty(filepath.Dir(metaPath), s.meta)
	}
	if err := os.Remove(journalPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(s.transactions)
}

func syncDir(path string) (err error) {
	dir, err := os.Open(filepath.Clean(path))
	if err != nil {
		return err
	}
	defer func() { err = closeAfter(err, dir) }()
	return dir.Sync()
}

// ensureDirDurable creates a directory tree and fsyncs every level through root,
// including parents whose directory entries were added by MkdirAll.
func ensureDirDurable(path, root string) error {
	if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return errors.New("directory escapes root")
	}
	if err := os.MkdirAll(path, 0o750); err != nil {
		return err
	}
	for dir := path; ; dir = filepath.Dir(dir) {
		if err := syncDir(dir); err != nil {
			return err
		}
		if dir == root {
			return nil
		}
	}
}

func syncNearestDir(path, root string) error {
	for path != root && strings.HasPrefix(path, root+string(filepath.Separator)) {
		if err := syncDir(path); err == nil {
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		path = filepath.Dir(path)
	}
	return syncDir(root)
}
