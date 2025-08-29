package main

import (
	"flag"

	"github.com/josuemontano/ollama-copilot/internal"
	"go.uber.org/zap"
)

var logger *zap.Logger

var (
	port              = flag.String("port", ":11437", "Port to listen on")
	proxyPort         = flag.String("proxy-port", ":11438", "Proxy port to listen on")
	portSSL           = flag.String("port-ssl", ":11436", "Port to listen on")
	proxyPortSSL      = flag.String("proxy-port-ssl", ":11435", "Proxy port to listen on")
	cert              = flag.String("cert", "", "Certificate file path *.crt")
	key               = flag.String("key", "", "Key file path *.key")
	model             = flag.String("model", "qwen3-coder:30b", "LLM model to use")
	numPredict        = flag.Int("num-predict", 200, "Maximum number of tokens to predict")
	promptTemplateStr = flag.String("prompt-template", "<|fim_prefix|> {{.Prefix}} <|fim_suffix|>{{.Suffix}} <|fim_middle|>", "Fill-in-middle template to apply in prompt")
	verbose           = flag.Bool("verbose", false, "Enable verbose mode")
)

// main is the entrypoint for the program.
func main() {
	flag.Parse()

	if *verbose {
		logger, _ = zap.NewDevelopment()
	} else {
		logger, _ = zap.NewProduction()
	}
	defer logger.Sync()

	server := &internal.Server{
		PortSSL:     *portSSL,
		Port:        *port,
		Certificate: *cert,
		Key:         *key,
		Template:    *promptTemplateStr,
		Model:       *model,
		NumPredict:  *numPredict,
		Logger:      logger,
	}

	go internal.Proxy(*proxyPortSSL, *portSSL)
	go internal.Proxy(*proxyPort, *port)

	go server.Serve()
	server.ServeTLS()
}
