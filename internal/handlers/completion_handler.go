package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	prompt, err := Prompt{Prefix: req.Prompt, Suffix: req.Suffix}.Generate(ch.promptTmpl)
	if err != nil {
		return err
	}

	systemBuf := bytes.Buffer{}
	if err := ch.systemTmpl.Execute(&systemBuf, struct{ Language string }{Language: req.Extra.Language}); err != nil {
		return fmt.Errorf("executing system template: %w", err)
	}

	numPredict := req.MaxTokens
	if ch.numPredict < numPredict {
		numPredict = ch.numPredict
	}
	genReq := api.GenerateRequest{
		Model:  ch.model,
		Prompt: prompt,
		System: systemBuf.String(),
		Options: map[string]interface{}{
			"temperature": req.Temperature,
			"top_p":       req.TopP,
			"stop":        req.Stop,
			"num_predict": numPredict,
		},
	}

	done := make(chan struct{})
	err = ch.api.Generate(ctx, &genReq, func(resp api.GenerateResponse) error {
		ch.logger.Debug("Chunk generated", zap.String("chunk", resp.Response))

		response := CompletionResponse{
			Id:      uuid.New().String(),
			Created: time.Now().Unix(),
			Choices: []ChoiceResponse{{Text: resp.Response, Index: 0}},
		}

		// SSE formatting
		if _, err := fmt.Fprintf(w, "data: "); err != nil {
			return err
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "\n\n"); err != nil {
			return err
		}

		if resp.Done {
			close(done)
		}
		return nil
	})

	if err == nil {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-done:
		}
	}

	return err
}
