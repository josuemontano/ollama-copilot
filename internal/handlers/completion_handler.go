package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/google/uuid"
	"github.com/ollama/ollama/api"
	"go.uber.org/zap"
)

// CompletionRequest represents the request sent to the completion handler.
type CompletionRequest struct {
	Extra struct {
		Language          string `json:"language"`
		NextIndent        int    `json:"next_indent"`
		PromptTokens      int    `json:"prompt_tokens"`
		SuffixTokens      int    `json:"suffix_tokens"`
		TrimByIndentation bool   `json:"trim_by_indentation"`
	} `json:"extra"`
	MaxTokens   int      `json:"max_tokens"`
	N           int      `json:"n"`
	Prompt      string   `json:"prompt"`
	Stop        []string `json:"stop"`
	Stream      bool     `json:"stream"`
	Suffix      string   `json:"suffix"`
	Temperature float64  `json:"temperature"`
	TopP        int      `json:"top_p"`
}

// ChoiceResponse is a single completion choice.
type ChoiceResponse struct {
	Text         string `json:"text"`
	Index        int    `json:"index"`
	FinishReason string `json:"finish_reason,omitempty"`
}

// CompletionResponse is the full response returned to the client.
type CompletionResponse struct {
	Id      string           `json:"id"`
	Created int64            `json:"created"`
	Choices []ChoiceResponse `json:"choices"`
}

// Prompt represents a FIM prompt with prefix/suffix.
type Prompt struct {
	Prefix string
	Suffix string
}

// Generate executes the prompt template.
func (p Prompt) Generate(tmpl *template.Template) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p); err != nil {
		return "", fmt.Errorf("executing prompt template: %w", err)
	}
	return buf.String(), nil
}

// CompletionHandler streams completions from Ollama.
type CompletionHandler struct {
	api        *api.Client
	model      string
	promptTmpl *template.Template
	systemTmpl *template.Template
	numPredict int
	logger     *zap.Logger
}

// NewCompletionHandler constructs a new CompletionHandler.
func NewCompletionHandler(api *api.Client, model string, promptTmpl *template.Template, numPredict int, logger *zap.Logger) *CompletionHandler {
	systemTmpl := template.Must(template.New("system").Parse(
		`You are an expert AI programming assistant for {{.Language}}. 
Your goal is to perform Fill-in-the-Middle (FIM) code completion. Complete only the code that fits between the given prefix and suffix. 
Do not add explanations, comments, or markdown. Do not change code outside the specified boundaries.`))

	return &CompletionHandler{
		api:        api,
		model:      model,
		promptTmpl: promptTmpl,
		systemTmpl: systemTmpl,
		numPredict: numPredict,
		logger:     logger,
	}
}

// ServeHTTP handles completion requests.
func (ch *CompletionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req CompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ch.logger.Error("Failed to decode request", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	ch.logger.Debug("Incoming completion request", zap.Any("request", req))
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()

	if err := ch.generateCompletion(ctx, w, req); err != nil {
		ch.logger.Error("Completion generation failed", zap.Error(err))
	}
}

// generateCompletion streams a code completion from Ollama.
func (ch *CompletionHandler) generateCompletion(ctx context.Context, w http.ResponseWriter, req CompletionRequest) error {
	startTime := time.Now()

	prefix, suffix := getLinesAroundCursor(req.Prompt, req.Suffix, 60, 60)
	prompt, err := Prompt{Prefix: prefix, Suffix: suffix}.Generate(ch.promptTmpl)
	if err != nil {
		return err
	}

	systemBuf := bytes.Buffer{}
	if err := ch.systemTmpl.Execute(&systemBuf, struct{ Language string }{Language: req.Extra.Language}); err != nil {
		return fmt.Errorf("executing system template: %w", err)
	}

	numPredict := minInt(req.MaxTokens, ch.numPredict)
	stopTokens := ensureImEndStop(req.Stop)
	genReq := api.GenerateRequest{
		Model:  ch.model,
		Prompt: prompt,
		System: systemBuf.String(),
		Options: map[string]interface{}{
			"temperature": req.Temperature,
			"top_p":       req.TopP,
			"stop":        stopTokens,
			"num_predict": numPredict,
		},
	}

	done := make(chan struct{})
	var genErr error
	var totalChunks []string
	var prevSkipped bool

	// Always return nil error so the stream ends gracefully
	_ = ch.api.Generate(ctx, &genReq, func(resp api.GenerateResponse) error {
		// Skip chunks that are exactly "```" or "python"
		trimmed := strings.TrimSpace(resp.Response)
		if trimmed == "```" || trimmed == "python" {
			prevSkipped = true
			return nil
		}

		chunk := resp.Response
		// If previous was skipped and current starts with newline, remove leading newline
		if prevSkipped && strings.HasPrefix(chunk, "\n") {
			chunk = strings.TrimPrefix(chunk, "\n")
		}
		prevSkipped = false

		ch.logger.Debug("Chunk generated", zap.Any("chunk", resp))
		totalChunks = append(totalChunks, chunk)

		response := CompletionResponse{
			Id:      uuid.New().String(),
			Created: time.Now().Unix(),
			Choices: []ChoiceResponse{{Text: chunk, Index: 0}},
		}

		if _, err := fmt.Fprintf(w, "data: "); err != nil {
			ch.logger.Warn("Failed to write SSE prefix", zap.Error(err))
			return nil
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			ch.logger.Warn("Failed to write SSE response", zap.Error(err))
			return nil
		}
		if _, err := fmt.Fprintf(w, "\n\n"); err != nil {
			ch.logger.Warn("Failed to write SSE suffix", zap.Error(err))
			return nil
		}

		if resp.Done {
			close(done)
		}

		return nil
	})

	// Wait for either context timeout or done signal
	select {
	case <-ctx.Done():
		genErr = ctx.Err()
	case <-done:
		genErr = nil
	}

	// If there was an error, send a final "empty" chunk with durations
	if genErr != nil {
		endTime := time.Now()
		finalChunk := map[string]interface{}{
			"chunk": map[string]interface{}{
				"model":                ch.model,
				"created_at":           endTime.Format(time.RFC3339Nano),
				"response":             "",
				"done":                 true,
				"context":              []interface{}{},
				"total_duration":       endTime.Sub(startTime).Nanoseconds(),
				"load_duration":        0,
				"prompt_eval_count":    0,
				"prompt_eval_duration": 0,
				"eval_count":           0,
				"eval_duration":        0,
			},
		}
		ch.logger.Warn("Generator ended with error", zap.Error(genErr))

		fmt.Fprintf(w, "data: ")
		_ = json.NewEncoder(w).Encode(finalChunk)
		fmt.Fprintf(w, "\n\n")

	}

	return nil
}

// getLinesAroundCursor returns up to `before` lines from the end of prefix
// and up to `after` lines from the start of suffix.
func getLinesAroundCursor(prefixText, suffixText string, before, after int) (string, string) {
	prefixLines := strings.Split(prefixText, "\n")
	n := len(prefixLines)
	start := 0
	if n > before {
		start = n - before
	}
	prefix := strings.Join(prefixLines[start:], "\n")

	suffixLines := strings.Split(suffixText, "\n")
	if len(suffixLines) > after {
		suffixLines = suffixLines[:after]
	}
	suffix := strings.Join(suffixLines, "\n")

	return prefix, suffix
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func ensureImEndStop(stop []string) []string {
	for _, tok := range stop {
		if tok == "<|im_end|>" {
			return stop // already present
		}
	}
	return append(stop, "<|im_end|>")
}
