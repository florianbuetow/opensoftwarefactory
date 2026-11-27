package main

import (
	"fmt"

	"github.com/google/uuid"
)

// newRunID returns the id of a new run: a version 7 UUID, the current time in milliseconds
// followed by random bits. Every run gets its id here and nowhere else, so an id is never
// derived from what started the run and never given to a second run.
func newRunID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("generating a run id: %w", err)
	}
	return id.String(), nil
}
