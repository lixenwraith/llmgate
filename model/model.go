// Package model asks the model server. Ollama is the one backend so far:
// its chat API, greedy, the model kept loaded so that requests sharing a
// prefix reuse its prompt cache. An app sees only the Model interface.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is one chat: Format is the JSON Schema the answer must meet, nil
// for free text; MaxTokens bounds the answer
type Request struct {
	Messages  []Message
	Format    json.RawMessage
	MaxTokens int
}

// Answer is the model's text and what it cost: PromptTokens are the prompt
// tokens evaluated, which a cache hit keeps near the request's new part
type Answer struct {
	Content      string
	PromptTokens int
	AnswerTokens int
}

// ErrCut is an answer stopped at its token limit, never used as a partial
var ErrCut = errors.New("answer cut at its token limit")

type Model interface {
	Name() string
	Chat(ctx context.Context, r Request) (Answer, error)
	Ready(ctx context.Context) error
}

// Ollama is a model served by Ollama. Context and Threads are sent with
// every request: a change of either reloads the model. Think, when set, is
// sent as is, false for a model that thinks unasked.
type Ollama struct {
	URL     string
	Model   string
	Context int
	Threads int
	Think   *bool
	Client  *http.Client
}

func (o *Ollama) Name() string { return o.Model }

func (o *Ollama) Chat(ctx context.Context, r Request) (Answer, error) {
	body := map[string]any{"model": o.Model, "messages": r.Messages, "stream": false, "keep_alive": -1,
		"options": map[string]any{"temperature": 0, "top_k": 1, "seed": 1, "num_predict": r.MaxTokens,
			"num_ctx": o.Context, "num_thread": o.Threads}}
	if r.Format != nil {
		body["format"] = r.Format
	}
	if o.Think != nil {
		body["think"] = *o.Think
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		DoneReason      string `json:"done_reason"`
		PromptEvalCount int    `json:"prompt_eval_count"`
		EvalCount       int    `json:"eval_count"`
	}
	if err := o.call(ctx, http.MethodPost, "/api/chat", body, &out); err != nil {
		return Answer{}, err
	}
	a := Answer{out.Message.Content, out.PromptEvalCount, out.EvalCount}
	if out.DoneReason == "length" {
		return a, ErrCut
	}
	return a, nil
}

// Ready asks the server it answers, and has the model
func (o *Ollama) Ready(ctx context.Context) error {
	return o.call(ctx, http.MethodPost, "/api/show", map[string]any{"model": o.Model}, nil)
}

func (o *Ollama) call(ctx context.Context, method, path string, in, out any) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, o.URL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := o.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(body, &e)
		return fmt.Errorf("%s %s: %d %s", method, path, res.StatusCode, e.Error)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}
