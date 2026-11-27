package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

import "opensoftwarefactory/orchestrator/pipeline"

func TestMigrateErrors(t *testing.T) {
	if _, err := migrate([]byte("unknown_field: 1\n")); err == nil {
		t.Fatal("expected unknown field error")
	}
	if _, err := migrate([]byte("workflows:\n  - name: bad\n    steps: []\n")); err == nil {
		t.Fatal("expected validation error for empty steps")
	}
}

func TestStoreAllErrors(t *testing.T) {
	workflows, err := migrate([]byte("workflows:\n  - name: w1\n    steps: [{id: a, service: svc}]\n"))
	if err != nil {
		t.Fatal(err)
	}

	// Non-404 GET error.
	getErrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer getErrSrv.Close()
	store := pipeline.HTTPStore{BaseURL: getErrSrv.URL + "/blobs", Client: getErrSrv.Client()}
	var out bytes.Buffer
	if err := storeAll(context.Background(), store, workflows, &out); err == nil {
		t.Fatal("expected GET error")
	}

	// 404 on GET followed by 500 on PUT.
	putErrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer putErrSrv.Close()
	store2 := pipeline.HTTPStore{BaseURL: putErrSrv.URL + "/blobs", Client: putErrSrv.Client()}
	if err := storeAll(context.Background(), store2, workflows, &out); err == nil {
		t.Fatal("expected PUT error")
	}
}
