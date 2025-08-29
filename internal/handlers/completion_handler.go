package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"text/template"
	"time"

	"github.com/google/uuid"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/ollama"
	"go.uber.org/zap"
)

var logger *zap.Logger

// CompletionRequest is the request sent to the completion handler
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

// Logprobs is the logprobs returned by the CompletionResponse
type Logprobs struct {
	Tokens []struct {
		Token   string  `json:"token"`
		Logprob float64 `json:"logprob"`
	} `json:"tokens"`
}

// ChoiceResponse is the response returned CompletionResponse
type ChoiceResponse struct {
	Text         string `json:"text"`
	Index        int    `json:"index"`
	Logprobs     *Logprobs
	FinishReason string `json:"finish_reason"`
}

// CompletionResponse is the response returned by the CompletionHandler
type CompletionResponse struct {
	Id      string           `json:"id"`
	Created int64            `json:"created"`
	Choices []ChoiceResponse `json:"choices"`
}

// Prompt is an repreentation of a prompt with suffi and prefix
type Prompt struct {
	Prefix string
	Suffix string
}

func (p Prompt) Generate(tmpl *template.Template) string {
	var buf = new(bytes.Buffer)
	err := tmpl.Execute(buf, p)

	if err != nil {
		logger.Fatal("Error parsing the prompt template", zap.Error(err))
	}

	return buf.String()
}

// System is a representation of the system
type System struct {
	Language string
}

func (s System) Generate() string {
	const tmplStr = "You are an expert AI programming assistant for {{.Language}}. You write simple, concise code. Your task is to Fill-in-the-middle (FIM) or infill. Only output the code completion without any preamble, explanation, or markdown formatting."
	tmpl, err := template.New("system").Parse(tmplStr)

	if err != nil {
		logger.Error("Error compiling the system template", zap.Error(err))
		return ""
	}

	var buf bytes.Buffer
	err = tmpl.Execute(&buf, s)
	if err != nil {
		logger.Error("Error parsing the system template", zap.Error(err))
		return ""
	}

	return buf.String()
}

// CompletionHandler is an http.Handler that returns completions.
type CompletionHandler struct {
	model      string
	templ      *template.Template
	numPredict int
}

// NewCompletionHandler returns a new CompletionHandler.
func NewCompletionHandler(model string, template *template.Template, numPredict int) *CompletionHandler {
	return &CompletionHandler{model, template, numPredict}
}

// ServeHTTP implements http.Handler.
func (c *CompletionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	req := CompletionRequest{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Fatal("Error decoding request", zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	w.Header().Set("content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	// Prepare system + prompt
	systemPrompt := System{Language: req.Extra.Language}.Generate()
	taskPrompt := Prompt{Prefix: req.Prompt, Suffix: req.Suffix}.Generate(c.templ)

	ctx, cancel := context.WithTimeout(r.Context(), time.Second*60)
	defer cancel()

	// Initialize Ollama via LangChainGo
	llm, err := ollama.New(ollama.WithModel(c.model))
	if err != nil {
		logger.Fatal("Failed to intialize the Ollama client", zap.Error(err))
		http.Error(w, "Failed to intialize the Ollama client", http.StatusInternalServerError)
		return
	}

	doneChan := make(chan struct{})

	_, err = llms.GenerateFromSinglePrompt(
		ctx,
		llm,
		systemPrompt+"\n\n"+taskPrompt,
		llms.WithTemperature(req.Temperature),
		llms.WithStreamingFunc(func(_ context.Context, chunk []byte) error {
			if chunk == nil {
				return nil
			}

			resp := CompletionResponse{
				Id:      uuid.New().String(),
				Created: time.Now().Unix(),
				Choices: []ChoiceResponse{
					{
						Text:  string(chunk),
						Index: 0,
					},
				},
			}

			logger.Debug("Chunk generated", zap.ByteString("chunk", chunk), zap.Any("response", resp))

			_, err := w.Write([]byte("data: "))
			if err != nil {
				cancel()
				return err
			}
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				cancel()
				return err
			}
			return nil
		}),
	)
	close(doneChan)

	if err == nil {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-doneChan:
		}
	}

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		logger.Error("Generation failed", zap.Error(err))
		return
	}
}
