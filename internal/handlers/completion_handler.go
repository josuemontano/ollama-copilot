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
	Logger *zap.Logger
}

func (p Prompt) Generate(tmpl *template.Template) string {
	var buf = new(bytes.Buffer)
	err := tmpl.Execute(buf, p)

	if err != nil {
		p.Logger.Fatal("Error parsing the prompt template", zap.Error(err))
	}

	return buf.String()
}

// System is a representation of the system
type System struct {
	Language string
	Logger   *zap.Logger
}

func (s System) Generate() string {
	const tmplStr = "You are an expert AI programming assistant for {{.Language}}. You write simple, concise code. Your task is to Fill-in-the-middle (FIM) or infill. Only output the code completion without any preamble, explanation, or markdown formatting."
	tmpl, err := template.New("system").Parse(tmplStr)

	if err != nil {
		s.Logger.Error("Error compiling the system template", zap.Error(err))
		return ""
	}

	var buf bytes.Buffer
	err = tmpl.Execute(&buf, s)
	if err != nil {
		s.Logger.Error("Error parsing the system template", zap.Error(err))
		return ""
	}

	return buf.String()
}

// CompletionHandler is an http.Handler that returns completions.
type CompletionHandler struct {
	api        *api.Client
	model      string
	templ      *template.Template
	numPredict int
	logger     *zap.Logger
}

// NewCompletionHandler returns a new CompletionHandler.
func NewCompletionHandler(api *api.Client, model string, promptTemplate *template.Template, numPredict int, logger *zap.Logger) *CompletionHandler {
	return &CompletionHandler{api, model, promptTemplate, numPredict, logger}
}

// ServeHTTP implements http.Handler.
func (ch *CompletionHandler) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		responseWriter.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	req := CompletionRequest{}
	if err := json.NewDecoder(request.Body).Decode(&req); err != nil {
		ch.logger.Error("Error decoding request", zap.Error(err))
		responseWriter.WriteHeader(http.StatusBadRequest)
		return
	}

	responseWriter.Header().Set("content-Type", "application/json")
	responseWriter.WriteHeader(http.StatusOK)

	generate := api.GenerateRequest{
		Model:  ch.model,
		Prompt: Prompt{Prefix: req.Prompt, Suffix: req.Suffix, Logger: ch.logger}.Generate(ch.templ),
		System: System{Language: req.Extra.Language, Logger: ch.logger}.Generate(),
		Options: map[string]interface{}{
			"temperature": req.Temperature,
			"top_p":       req.TopP,
			"stop":        req.Stop,
			"num_predict": ch.numPredict,
		},
	}

	ctx, cancel := context.WithTimeout(request.Context(), time.Second*60)
	request = request.WithContext(ctx)
	defer cancel()

	doneChan := make(chan struct{})
	err := ch.api.Generate(request.Context(), &generate, func(resp api.GenerateResponse) error {
		response := CompletionResponse{
			Id:      uuid.New().String(),
			Created: time.Now().Unix(),
			Choices: []ChoiceResponse{
				{
					Text:  resp.Response,
					Index: 0,
				},
			},
		}
		ch.logger.Debug("Chunk generated", zap.Any("response", resp))

		_, err := responseWriter.Write([]byte("data: "))
		if err != nil {
			cancel()
			return err
		}

		err = json.NewEncoder(responseWriter).Encode(response)
		if err != nil {
			cancel()
			return err
		}
		if resp.Done {
			close(doneChan)
		}

		return nil
	})

	if err == nil {
		select {
		case <-request.Context().Done():
			err = request.Context().Err()
		case <-doneChan:
		}
	}

	if err != nil {
		responseWriter.WriteHeader(http.StatusInternalServerError)
		ch.logger.Error("Generation failed", zap.Error(err))
		return
	}
}
