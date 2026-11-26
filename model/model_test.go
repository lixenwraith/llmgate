package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every chat is greedy, keeps the model loaded and sends the same context
// and threads, so that requests share one loaded model and its prompt cache
func TestOllamaChatsAlikeAndReportsACut(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": `{"flags":[`},
			"done_reason": "length", "prompt_eval_count": 12, "eval_count": 8})
	}))
	defer srv.Close()
	off := false
	o := &Ollama{URL: srv.URL, Model: "m", Context: 6144, Threads: 4, Think: &off, Client: srv.Client()}
	a, err := o.Chat(context.Background(), Request{Messages: []Message{{"user", "hi"}}, Format: json.RawMessage(`{"type":"object"}`), MaxTokens: 320})
	if !errors.Is(err, ErrCut) || a.PromptTokens != 12 || a.AnswerTokens != 8 {
		t.Errorf("a cut answer: %+v, %v", a, err)
	}
	opts, _ := got["options"].(map[string]any)
	for k, want := range map[string]any{"temperature": 0.0, "top_k": 1.0, "num_ctx": 6144.0, "num_thread": 4.0, "num_predict": 320.0} {
		if opts[k] != want {
			t.Errorf("options.%s = %v, want %v", k, opts[k], want)
		}
	}
	if got["keep_alive"] != -1.0 || got["stream"] != false || got["think"] != false || got["format"] == nil {
		t.Errorf("request %v", got)
	}
}
