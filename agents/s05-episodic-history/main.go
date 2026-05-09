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
//
// Inherited from s03-s04; s05 keeps the surface unchanged. The new
// thing in s05 is internal: the Loop now holds a `*History`, and the
// strategy's BuildPrompt receives prior episodes — but you don't see
// either from the CLI.
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
	verbose := flag.Bool("v", false, "print every turn (assistant text + tool calls + history growth)")
	maxTurns := flag.Int("max-turns", 20, "max agent turns before giving up")
	provider := flag.String("provider", envOr("PROVIDER", "anthropic"),
		"provider profile: anthropic | openai | deepseek | moonshot | qwen | groq | openrouter | local")
	baseURL := flag.String("base-url", envOr("BASE_URL", ""),
		"override the OpenAI-compatible base URL (e.g. http://localhost:8000/v1)")
	modelFlag := flag.String("model", envOr("MODEL", ""),
		"override the model id (defaults to the provider profile's default)")
	strategyFlag := flag.String("strategy", envOr("STRATEGY", "oneshot"),
		"prompt strategy: oneshot (only option in s04+; s10 adds reflexion)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: s05-episodic-history [-v] [-provider P] [-base-url URL] [-model ID] [-strategy S] <prompt>\n\n"+
				"  Provider profiles (set the matching API key env var first):\n"+
				"    anthropic  → ANTHROPIC_API_KEY     (Claude)\n"+
				"    openai     → OPENAI_API_KEY        (gpt-4o-mini default)\n"+
				"    deepseek   → DEEPSEEK_API_KEY      (deepseek-chat / deepseek-reasoner)\n"+
				"    moonshot   → MOONSHOT_API_KEY      (Kimi / moonshot-v1-8k)\n"+
				"    qwen       → DASHSCOPE_API_KEY     (Qwen via DashScope OpenAI-compat)\n"+
				"    groq       → GROQ_API_KEY          (llama-3.3-70b default)\n"+
				"    openrouter → OPENROUTER_API_KEY    (any model on OpenRouter)\n"+
				"    local      → http://localhost:8000/v1 (vLLM/SGLang etc.)\n\n"+
				"  Strategies:\n"+
				"    oneshot    → directives + tool list in system prompt; native tool_use OR JSON-fence fallback parsing\n\n"+
				"  s05 adds episodic history: the Loop appends an Episode per tool turn,\n"+
				"  the strategy renders prior episodes back into the prompt on each new turn.\n\n"+
				"  Examples:\n"+
				"    s05-episodic-history -v \"add 2 and 3, then echo the result\"\n"+
				"    s05-episodic-history -provider deepseek -v \"echo hi, then echo bye\"\n"+
				"    s05-episodic-history -provider local -model llama-3.3 \"...\"\n")
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

	var strategy PromptStrategy
	switch *strategyFlag {
	case "oneshot":
		strategy = NewOneShotStrategy()
	default:
		log.Fatalf("unknown -strategy %q (s05 ships only 'oneshot'; s10 adds reflexion)", *strategyFlag)
	}

	reg := NewRegistry()
	if err := reg.Register(NewEchoTool()); err != nil {
		log.Fatalf("register echo: %v", err)
	}
	if err := reg.Register(NewMathTool()); err != nil {
		log.Fatalf("register math: %v", err)
	}

	// s05 payoff: construct an empty History and let the Loop grow it
	// turn by turn. The Loop is nil-safe — `&Loop{...}` without History
	// works too — but constructing it explicitly here mirrors the
	// upstream `Agent.event_history = EpisodicActionHistory(...)` setup
	// and is what tests will most often want to do.
	history := &History{}

	loop := &Loop{
		Provider: p,
		Tools:    reg,
		Strategy: strategy,
		History:  history,
		MaxTurns: *maxTurns,
		Verbose:  *verbose,
	}
	if *verbose {
		fmt.Fprintf(os.Stderr, "[s05-episodic-history] provider=%s model=%s url=%s strategy=%s tools=%d\n",
			*provider, model, url, *strategyFlag, len(reg.All()))
	}

	final, err := loop.Run(context.Background(), prompt)
	if err != nil {
		log.Fatalf("loop error: %v", err)
	}
	if *verbose {
		fmt.Fprintf(os.Stderr, "[s05-episodic-history] final history: %d episodes\n", len(*history))
	}
	fmt.Println(final)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
