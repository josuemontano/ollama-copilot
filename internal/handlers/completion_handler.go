package handlers

import (
	"bytes"
	"context"
	"encoding/json"
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

// Logprobs represents token log probabilities.
type Logprobs struct {
	Tokens []struct {
		Token   string  `json:"token"`
		Logprob float64 `json:"logprob"`
	} `json:"tokens"`
}

// ChoiceResponse is a single completion choice.
type ChoiceResponse struct {
	Text         string `json:"text"`
	Index        int    `json:"index"`
	Logprobs     *Logprobs
	FinishReason string `json:"finish_reason"`
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

func (p Prompt) Generate(tmpl *template.Template) string {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p); err != nil {
		panic("Error executing prompt template: " + err.Error())
	}
	return buf.String()
}

// System represents system instructions for the LLM.
type System struct {
	Language string
}

func (s System) Generate() string {
	const tmplStr = "You are an expert AI programming assistant for {{.Language}}. You write simple, concise code. Your task is to Fill-in-the-middle (FIM) or infill. Only output the code completion without any preamble, explanation, or markdown formatting."
	tmpl, err := template.New("system").Parse(tmplStr)
	if err != nil {
		panic("Error parsing system template: " + err.Error())
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, s); err != nil {
		panic("Error executing system template: " + err.Error())
	}

	return buf.String()
}

// CompletionHandler streams completions from Ollama.
type CompletionHandler struct {
	api        *api.Client
	model      string
	template   *template.Template
	numPredict int
	logger     *zap.Logger
}

// NewCompletionHandler constructs a new CompletionHandler.
func NewCompletionHandler(api *api.Client, model string, tmpl *template.Template, numPredict int, logger *zap.Logger) *CompletionHandler {
	return &CompletionHandler{
		api:        api,
		model:      model,
		template:   tmpl,
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

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	generateReq := api.GenerateRequest{
		Model:  ch.model,
		Prompt: Prompt{Prefix: req.Prompt, Suffix: req.Suffix}.Generate(ch.template),
		System: System{Language: req.Extra.Language}.Generate(),
		Options: map[string]interface{}{
			"temperature": req.Temperature,
			"top_p":       req.TopP,
			"stop":        req.Stop,
			"num_predict": ch.numPredict,
		},
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()

	done := make(chan struct{})
	err := ch.api.Generate(ctx, &generateReq, func(resp api.GenerateResponse) error {
		ch.logger.Debug("Chunk generated", zap.Any("chunk", resp.Response))

		response := CompletionResponse{
			Id:      uuid.New().String(),
			Created: time.Now().Unix(),
			Choices: []ChoiceResponse{{Text: resp.Response, Index: 0}},
		}

		if _, err := w.Write([]byte("data: ")); err != nil {
			cancel()
			return err
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			cancel()
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

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		ch.logger.Error("Completion generation failed", zap.Error(err))
	}
}
