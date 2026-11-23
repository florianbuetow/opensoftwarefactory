package main

import (
	"sync"
	"testing"
)

func TestEstimateTokensCountsCl100kBaseTokens(t *testing.T) {
	if estimationMethod != "tiktoken:cl100k_base" {
		t.Fatalf("estimation method = %q", estimationMethod)
	}
	for _, test := range []struct {
		input string
		want  int64
	}{
		{"", 0},
		{"hello", 1},
		{"hello world", 2},
		{"Hello, world!", 4},
		{"tiktoken is great!", 6},
		// A special token name in the text is counted as ordinary text.
		{"<|endoftext|>", 7},
	} {
		if got := estimateTokens(test.input); got != test.want {
			t.Errorf("estimateTokens(%q) = %d, want %d", test.input, got, test.want)
		}
	}
}

func TestEstimateJSONTokensCountsTheSerializedValue(t *testing.T) {
	value := map[string]string{"role": "user", "content": "hello world"}
	if got, want := estimateJSONTokens(value), estimateTokens(`{"content":"hello world","role":"user"}`); got != want || got == 0 {
		t.Fatalf("estimateJSONTokens = %d, want %d", got, want)
	}
	if got := estimateJSONTokens(func() {}); got != 0 {
		t.Fatalf("a value that cannot be serialized estimated %d tokens", got)
	}
}

func TestEstimateTokensIsSafeForConcurrentUse(t *testing.T) {
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if got := estimateTokens("hello world"); got != 2 {
					t.Errorf("estimateTokens = %d, want 2", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}
