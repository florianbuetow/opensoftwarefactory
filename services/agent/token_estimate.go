package main

import (
	"encoding/json"
	"sync"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

// estimationMethod names the tokenizer of the estimates. It is not provider usage.
const estimationMethod = "tiktoken:cl100k_base"

// tokenEncoder builds the cl100k_base encoder once. The vocabulary is embedded in the
// binary, so nothing is downloaded at run time.
var tokenEncoder = sync.OnceValue(func() *tiktoken.Tiktoken {
	tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	encoder, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		panic("loading the embedded cl100k_base encoding: " + err.Error())
	}
	return encoder
})

// estimateTokens counts the cl100k_base tokens of text. Special token names in the text
// count as ordinary text.
func estimateTokens(text string) int64 {
	if text == "" {
		return 0
	}
	return int64(len(tokenEncoder().EncodeOrdinary(text)))
}

func estimateJSONTokens(v any) int64 {
	data, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return estimateTokens(string(data))
}
