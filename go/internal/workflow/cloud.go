package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type CloudRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}
type CloudResponse struct {
	Content    string  `json:"content"`
	Tokens     *Tokens `json:"tokens"`
	DoneReason string  `json:"doneReason"`
}

// CloudWorker runs inside sandboxd. The loopback daemon owns the cloud sign-in;
// no Ollama credential is passed to the sandbox or printed by this worker.
func CloudWorker(ctx context.Context, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var input CloudRequest
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"model": input.Model, "messages": input.Messages, "stream": false, "format": "json", "think": false, "options": map[string]int{"num_predict": 4096}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:11434/api/chat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("Ollama cloud request returned HTTP %d", res.StatusCode)
	}
	var result struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Input      *int64 `json:"prompt_eval_count"`
		Output     *int64 `json:"eval_count"`
		Cache      *int64 `json:"prompt_eval_cached_count"`
		DoneReason string `json:"done_reason"`
		Error      string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&result); err != nil {
		return err
	}
	if result.Error != "" {
		return errors.New("Ollama returned a model error")
	}
	return json.NewEncoder(os.Stdout).Encode(CloudResponse{Content: result.Message.Content, Tokens: &Tokens{Input: result.Input, Output: result.Output, CacheRead: result.Cache}, DoneReason: result.DoneReason})
}
