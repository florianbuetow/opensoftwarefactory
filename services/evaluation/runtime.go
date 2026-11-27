package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func openObserveCredentials(path string) (user, password string, err error) {
	configPath := filepath.Clean(path)
	file, err := os.Open(configPath)
	if err != nil {
		return "", "", err
	}
	defer func() { err = closeAfter(err, file) }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if !ok || strings.HasPrefix(key, "#") {
			continue
		}
		switch key {
		case "ZO_ROOT_USER_EMAIL":
			user = value
		case "ZO_ROOT_USER_PASSWORD":
			password = value
		}
	}
	err = scanner.Err()
	if err != nil {
		return "", "", err
	}
	if user == "" || password == "" {
		return "", "", errors.New("OpenObserve config omits root credentials")
	}
	return user, password, nil
}

type writerLock struct{ file *os.File }

func acquireWriterLock(path string) (*writerLock, error) {
	if path == "" {
		return nil, errors.New("EVALUATION_LOCK_PATH is required")
	}
	lockPath := filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o750); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("another evaluation writer owns %s: %w", path, err)
	}
	return &writerLock{file: file}, nil
}

func (l *writerLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	return l.file.Close()
}

func (s *service) projectorLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		_ = s.projectPending(ctx)
		_ = s.resumeBackfills(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *service) projectPending(ctx context.Context) error {
	version, readAt := s.pending.begin(), time.Now()
	ids, err := s.listObjects(ctx, operationPrefix)
	if err != nil {
		return err
	}
	generations := map[time.Time]int64{}
	unfinished := map[time.Time]bool{}
	for _, id := range ids {
		object, err := s.readObject(ctx, id)
		if err != nil {
			return err
		}
		var operation mutationOperation
		if json.Unmarshal(object.Data, &operation) != nil || operation.Phase == opComplete || operation.Phase == opCancelled {
			continue
		}
		unfinished[operation.AffectedHour] = true
		// A failed recovery is kept in the operation's error field and tried
		// again on the next pass.
		if recoverErr := s.recoverOperation(ctx, id, object.Revision, &operation); recoverErr != nil {
			continue
		}
		if operation.Phase != opProjectionPending {
			continue
		}
		hour := operation.AffectedHour.UTC().Truncate(time.Hour)
		if operation.Generation > generations[hour] {
			generations[hour] = operation.Generation
		}
	}
	// Series requests answer from this reading instead of reading every operation again.
	s.pending.store(version, unfinished, readAt)
	hours := make([]time.Time, 0, len(generations))
	for hour := range generations {
		hours = append(hours, hour)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i].Before(hours[j]) })
	if len(hours) > 2 {
		current := time.Now().UTC().Truncate(time.Hour)
		chosen := []time.Time{hours[0]}
		if generations[current] > 0 && current != hours[0] {
			chosen = append(chosen, current)
		} else {
			chosen = append(chosen, hours[len(hours)-1])
		}
		hours = chosen
	}
	for _, hour := range hours {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, _ = s.advanceProjection(ctx, hour, generations[hour])
	}
	return nil
}

// recoverOperation carries an operation forward from the phase it was left in
// and updates op as it goes, so the caller sees the phase it reached.
func (s *service) recoverOperation(ctx context.Context, id, revision string, op *mutationOperation) error {
	persist := func() error {
		op.UpdatedAt = time.Now().UTC()
		next, err := s.writeObject(ctx, id, op, revision)
		if err == nil {
			revision = next
		}
		return err
	}
	nextPhase := func() string {
		if strings.HasPrefix(op.ObjectID, usagePrefix+"/") {
			return opCalculationPending
		}
		return opProjectionPending
	}
	if op.Phase == opIntent {
		current, err := s.readObject(ctx, op.ObjectID)
		if err == nil && jsonEquivalent(current.Data, op.Replacement) {
			if strings.HasPrefix(op.ObjectID, "measurements/") {
				s.syncMeasurementToSQLDB(ctx, op.Replacement)
			} else if strings.HasPrefix(op.ObjectID, costPrefix+"/") {
				var cost CurrentCost
				if json.Unmarshal(op.Replacement, &cost) == nil {
					s.syncCostToSQLDB(ctx, &cost)
				}
			}
			op.Phase = nextPhase()
			op.Error = ""
			err = persist()
			if err != nil {
				return err
			}
		}
		if errors.Is(err, errNotFound) || err == nil && !jsonEquivalent(current.Data, op.Replacement) {
			_, writeErr := s.writeObject(ctx, op.ObjectID, op.Replacement, op.ExpectedRevision)
			if errors.Is(writeErr, errPrecondition) {
				latest, readErr := s.readObject(ctx, op.ObjectID)
				if readErr != nil || !jsonEquivalent(latest.Data, op.Replacement) {
					op.Phase = opConflict
					op.Error = "canonical precondition failed during recovery"
					_ = persist()
					return errPrecondition
				}
			}
			if writeErr != nil && !errors.Is(writeErr, errPrecondition) {
				op.Error = writeErr.Error()
				_ = persist()
				return writeErr
			}
			if strings.HasPrefix(op.ObjectID, "measurements/") {
				s.syncMeasurementToSQLDB(ctx, op.Replacement)
			} else if strings.HasPrefix(op.ObjectID, costPrefix+"/") {
				var cost CurrentCost
				if json.Unmarshal(op.Replacement, &cost) == nil {
					s.syncCostToSQLDB(ctx, &cost)
				}
			}
			op.Phase = nextPhase()
			op.Error = ""
			err = persist()
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	if op.Phase == opCalculationPending {
		measurement, err := s.loadMeasurement(ctx, kindUsage, op.MeasurementID)
		if err != nil {
			op.Error = err.Error()
			_ = persist()
			return err
		}
		cost, err := s.calculateCost(ctx, &measurement, "", op.Generation)
		if err != nil {
			op.Error = err.Error()
			_ = persist()
			return err
		}
		condition := "*"
		if current, readErr := s.readObject(ctx, costPrefix+"/"+op.MeasurementID+".json"); readErr == nil {
			var old CurrentCost
			if json.Unmarshal(current.Data, &old) == nil && old.Generation > op.Generation {
				op.Phase = opProjectionPending
				op.Error = ""
				_ = persist()
				return nil
			}
			condition = current.Revision
		} else if !errors.Is(readErr, errNotFound) {
			op.Error = readErr.Error()
			_ = persist()
			return readErr
		}
		op.CostReplacement = &cost
		op.CostExpectedRevision = condition
		op.Phase = opCostIntent
		op.Error = ""
		err = persist()
		if err != nil {
			return err
		}
	}
	if op.Phase == opCostIntent {
		_, err := s.writeObject(ctx, costPrefix+"/"+op.MeasurementID+".json", op.CostReplacement, op.CostExpectedRevision)
		if errors.Is(err, errPrecondition) {
			current, readErr := s.readObject(ctx, costPrefix+"/"+op.MeasurementID+".json")
			expected, _ := json.Marshal(op.CostReplacement)
			if readErr != nil || !jsonEquivalent(current.Data, expected) {
				op.Phase = opConflict
				op.Error = "cost precondition failed during recovery"
				_ = persist()
				return errPrecondition
			}
		}
		if err != nil && !errors.Is(err, errPrecondition) {
			op.Error = err.Error()
			_ = persist()
			return err
		}
		s.syncCostToSQLDB(ctx, op.CostReplacement)
		op.Phase = opProjectionPending
		op.Error = ""
		err = persist()
		if err != nil {
			return err
		}
	}
	return nil
}

func jsonEquivalent(a, b []byte) bool {
	var left, right any
	da := json.NewDecoder(strings.NewReader(string(a)))
	db := json.NewDecoder(strings.NewReader(string(b)))
	da.UseNumber()
	db.UseNumber()
	return da.Decode(&left) == nil && db.Decode(&right) == nil && reflect.DeepEqual(left, right)
}
