package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

// Provider profiles — convenient one-letter shortcuts for the popular
// OpenAI-compatible endpoints (and Anthropic's native API). Pass
// `-provider <name>` and we fill in the base URL and a default model.
// You can always override with `-base-url` and `-model`.
var providerProfiles = map[string]struct {
	BaseURL string
	Model   string
	APIKey  string // env var to read
}{
	"anthropic":  {Model: "claude-sonnet-4-6", APIKey: "ANTHROPIC_API_KEY"},
	"openai":     {BaseURL: "https://api.openai.com/v1", Model: "gpt-4o-mini", APIKey: "OPENAI_API_KEY"},
	"deepseek":   {BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat", APIKey: "DEEPSEEK_API_KEY"},
	"moonshot":   {BaseURL: "https://api.moonshot.cn/v1", Model: "moonshot-v1-8k", APIKey: "MOONSHOT_API_KEY"},
	"qwen":       {BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Model: "qwen-plus", APIKey: "DASHSCOPE_API_KEY"},
	"groq":       {BaseURL: "https://api.groq.com/openai/v1", Model: "llama-3.3-70b-versatile", APIKey: "GROQ_API_KEY"},
	"openrouter": {BaseURL: "https://openrouter.ai/api/v1", Model: "openai/gpt-4o-mini", APIKey: "OPENROUTER_API_KEY"},
	"local":      {BaseURL: "http://localhost:8000/v1", Model: "local-model", APIKey: "OPENAI_API_KEY"},
}

func main() {
	verbose := flag.Bool("v", false, "print every turn (assistant text + tool calls)")
	maxTurns := flag.Int("max-turns", 20, "max agent turns before giving up")
	provider := flag.String("provider", envOr("PROVIDER", "anthropic"),
		"provider profile: anthropic | openai | deepseek | moonshot | qwen | groq | openrouter | local")
	baseURL := flag.String("base-url", envOr("BASE_URL", ""),
		"override the OpenAI-compatible base URL (e.g. http://localhost:8000/v1)")
	modelFlag := flag.String("model", envOr("MODEL", ""),
		"override the model id (defaults to the provider profile's default)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: s02-command-registry [-v] [-provider P] [-base-url URL] [-model ID] <prompt>\n\n"+
				"  Provider profiles (set the matching API key env var first):\n"+
				"    anthropic  → ANTHROPIC_API_KEY     (Claude)\n"+
				"    openai     → OPENAI_API_KEY        (gpt-4o-mini default)\n"+
				"    deepseek   → DEEPSEEK_API_KEY      (deepseek-chat / deepseek-reasoner)\n"+
				"    moonshot   → MOONSHOT_API_KEY      (Kimi / moonshot-v1-8k)\n"+
				"    qwen       → DASHSCOPE_API_KEY     (Qwen via DashScope OpenAI-compat)\n"+
				"    groq       → GROQ_API_KEY          (llama-3.3-70b default)\n"+
				"    openrouter → OPENROUTER_API_KEY    (any model on OpenRouter)\n"+
				"    local      → http://localhost:8000/v1 (vLLM/SGLang etc.)\n\n"+
				"  Examples:\n"+
				"    s02-command-registry -v \"add 2 and 3\"                  # default Anthropic\n"+
				"    s02-command-registry -provider deepseek -v \"echo hi\"   # DeepSeek\n"+
				"    s02-command-registry -provider deepseek -model deepseek-reasoner \"...\"\n"+
				"    s02-command-registry -provider local -model llama-3.3 \"...\"\n")
	}
	flag.Parse()

	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	prompt := strings.Join(flag.Args(), " ")

	prof, ok := providerProfiles[*provider]
	if !ok {
		log.Fatalf("unknown -provider %q", *provider)
	}
	apiKey := os.Getenv(prof.APIKey)
	if apiKey == "" {
		log.Fatalf("%s is not set (required by -provider=%s)", prof.APIKey, *provider)
	}
	model := *modelFlag
	if model == "" {
		model = prof.Model
	}
	url := *baseURL
	if url == "" {
		url = prof.BaseURL
	}

	var p Provider
	switch *provider {
	case "anthropic":
		p = NewAnthropicProvider(apiKey, model)
	default:
		p = NewOpenAIProvider(apiKey, url, model)
	}

	// The s02 change: instead of `[]Tool{NewEchoTool()}`, build an explicit
	// Registry. Every Register call surfaces a dependency in source — so
	// what tools the agent has is visible without grep.
	reg := NewRegistry()
	if err := reg.Register(NewEchoTool()); err != nil {
		log.Fatalf("register echo: %v", err)
	}
	if err := reg.Register(NewMathTool()); err != nil {
		log.Fatalf("register math: %v", err)
	}

	loop := &Loop{
		Provider: p,
		Tools:    reg,
		MaxTurns: *maxTurns,
		Verbose:  *verbose,
	}
	if *verbose {
		fmt.Fprintf(os.Stderr, "[s02-command-registry] provider=%s model=%s url=%s tools=%d\n",
			*provider, model, url, len(reg.All()))
	}

	final, err := loop.Run(context.Background(), prompt)
	if err != nil {
		log.Fatalf("loop error: %v", err)
	}
	fmt.Println(final)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
