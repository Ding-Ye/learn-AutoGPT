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
// Inherited from s03 onward; s06 keeps the surface unchanged. The new
// thing in s06 is internal: a `LocalWorkspace` rooted at `./workspace/`
// is constructed at startup, and `read_file` + `write_file` tools join
// the registry alongside Echo and Math. From the CLI side you only see
// the agent finally being able to *do* something with the filesystem.
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
	verbose := flag.Bool("v", false, "print every turn (assistant text + tool calls + workspace path)")
	maxTurns := flag.Int("max-turns", 20, "max agent turns before giving up")
	provider := flag.String("provider", envOr("PROVIDER", "anthropic"),
		"provider profile: anthropic | openai | deepseek | moonshot | qwen | groq | openrouter | local")
	baseURL := flag.String("base-url", envOr("BASE_URL", ""),
		"override the OpenAI-compatible base URL (e.g. http://localhost:8000/v1)")
	modelFlag := flag.String("model", envOr("MODEL", ""),
		"override the model id (defaults to the provider profile's default)")
	strategyFlag := flag.String("strategy", envOr("STRATEGY", "oneshot"),
		"prompt strategy: oneshot (only option in s04+; s10 adds reflexion)")
	workspaceRoot := flag.String("workspace", envOr("WORKSPACE", "./workspace"),
		"workspace root directory (auto-mkdir if absent). All read_file / write_file paths are resolved relative to this.")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: s06-workspace [-v] [-provider P] [-base-url URL] [-model ID] [-strategy S] [-workspace DIR] <prompt>\n\n"+
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
				"  s06 adds a sandboxed Workspace + two file tools:\n"+
				"    read_file  → read a path RELATIVE to the workspace root\n"+
				"    write_file → write a path RELATIVE to the workspace root (parents auto-created)\n"+
				"  Absolute paths and `..` traversals are rejected by the LocalWorkspace sanitizer.\n\n"+
				"  Examples:\n"+
				"    s06-workspace -v \"create notes.md with a one-sentence summary of the agent loop\"\n"+
				"    s06-workspace -provider deepseek -v \"read README.md if it exists, then write a summary to notes.md\"\n"+
				"    s06-workspace -workspace /tmp/agent-out -v \"...\"\n")
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
		log.Fatalf("unknown -strategy %q (s06 ships only 'oneshot'; s10 adds reflexion)", *strategyFlag)
	}

	// s06 payoff: build a LocalWorkspace before the registry, then pass
	// it to the file tools as a constructor argument. The constructor
	// auto-mkdir's the root if it doesn't exist, so first-run users
	// don't trip over a missing-directory error.
	ws, err := NewLocalWorkspace(*workspaceRoot)
	if err != nil {
		log.Fatalf("workspace init: %v", err)
	}

	reg := NewRegistry()
	if err := reg.Register(NewEchoTool()); err != nil {
		log.Fatalf("register echo: %v", err)
	}
	if err := reg.Register(NewMathTool()); err != nil {
		log.Fatalf("register math: %v", err)
	}
	if err := reg.Register(NewReadFileTool(ws)); err != nil {
		log.Fatalf("register read_file: %v", err)
	}
	if err := reg.Register(NewWriteFileTool(ws)); err != nil {
		log.Fatalf("register write_file: %v", err)
	}

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
		fmt.Fprintf(os.Stderr, "[s06-workspace] provider=%s model=%s url=%s strategy=%s tools=%d workspace=%s\n",
			*provider, model, url, *strategyFlag, len(reg.All()), ws.Root())
	}

	final, err := loop.Run(context.Background(), prompt)
	if err != nil {
		log.Fatalf("loop error: %v", err)
	}
	if *verbose {
		fmt.Fprintf(os.Stderr, "[s06-workspace] final history: %d episodes\n", len(*history))
	}
	fmt.Println(final)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
