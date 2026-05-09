package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"
	"time"
)

// Provider profiles — same surface as s08. The s09 change is at the
// loop driver: instead of `loop.Run`, the binary calls
// `RunInteractionLoop` with a `LoopOpts{Cycles, AskEachStep}` and a
// `ConsoleUI` writing to stderr. Two new flags expose those:
//
//	-cycles N         continuous-mode budget (0 = infinite)
//	-ask-each-step    confirm each step via the configured Asker
var providerProfiles = map[string]struct {
	BaseURL string
	Model   string
	APIKey  string
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

type permissionsConfig struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

func main() {
	verbose := flag.Bool("v", false, "print every turn (assistant text + tool calls + permission decisions)")
	maxTurns := flag.Int("max-turns", 20, "max agent turns before giving up (safety bound, distinct from -cycles)")
	provider := flag.String("provider", envOr("PROVIDER", "anthropic"),
		"provider profile: anthropic | openai | deepseek | moonshot | qwen | groq | openrouter | local")
	baseURL := flag.String("base-url", envOr("BASE_URL", ""),
		"override the OpenAI-compatible base URL (e.g. http://localhost:8000/v1)")
	modelFlag := flag.String("model", envOr("MODEL", ""),
		"override the model id (defaults to the provider profile's default)")
	strategyFlag := flag.String("strategy", envOr("STRATEGY", "oneshot"),
		"prompt strategy: oneshot (only option in s04+; s10 adds reflexion)")
	workspaceRoot := flag.String("workspace", envOr("WORKSPACE", "./workspace"),
		"workspace root directory (auto-mkdir if absent)")
	permsPath := flag.String("permissions", envOr("PERMISSIONS", "./permissions.json"),
		"path to permissions.json (allow/deny rule list); falls back to built-in defaults if missing")
	askMode := flag.String("ask", envOr("ASK", "deny"),
		"behavior on Ask decisions: stdin (interactive y/N), allow (auto-allow), deny (auto-deny)")
	webTimeout := flag.Duration("web-timeout", 30*time.Second,
		"http timeout for the web_fetch component's outbound requests")
	cycles := flag.Int("cycles", 0,
		"continuous-mode cycle budget (0 = infinite, exit on Ctrl-C)")
	askEachStep := flag.Bool("ask-each-step", false,
		"prompt the operator before every step via the configured Asker")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: s09-continuous-mode [-v] [-cycles N] [-ask-each-step] [-provider P] [-strategy S]\n"+
				"                          [-workspace DIR] [-permissions FILE] [-ask MODE] [-web-timeout D] <prompt>\n\n"+
				"  s09 introduces continuous-mode autonomy with UI feedback:\n"+
				"    -cycles N         run up to N tool-use steps before exiting (0 = infinite)\n"+
				"    -ask-each-step    operator confirms every step via the Asker\n"+
				"    Ctrl-C exits cleanly; see README for the signal-handling pedagogy.\n\n"+
				"  Inherited from s08 (component-system):\n"+
				"    FileManagerComponent  → emits read_file, write_file + 'read-before-edit' directive\n"+
				"    WebFetchComponent     → emits web_fetch with -web-timeout\n\n"+
				"  Permission rules:\n"+
				"    -permissions FILE  JSON file with {\"allow\": [...], \"deny\": [...]}\n"+
				"    Pattern format     \"<command>: <arg-glob>\" — e.g. \"read_file: *.md\"\n"+
				"    Defaults (when FILE missing): allow read_file/write_file/web_fetch.\n\n"+
				"  Provider profiles, strategies, and workspace flags: see s06–s08.\n\n"+
				"  Example:\n"+
				"    s09-continuous-mode -v -cycles 5 \"fetch https://example.com and write a one-line summary to notes.md\"\n")
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
		log.Fatalf("unknown -strategy %q", *strategyFlag)
	}

	ws, err := NewLocalWorkspace(*workspaceRoot)
	if err != nil {
		log.Fatalf("workspace init: %v", err)
	}

	components := []Component{
		NewFileManagerComponent(ws),
		NewWebFetchComponent(*webTimeout),
	}
	bus := NewComponentBus(components...)

	perms, sourceLabel, err := loadPermissions(*permsPath)
	if err != nil {
		log.Fatalf("load permissions: %v", err)
	}

	var asker Asker
	switch *askMode {
	case "stdin":
		asker = NewStdinAsker()
	case "allow":
		asker = NewStubAsker(Allow)
	case "deny", "":
		asker = NewStubAsker(Deny)
	default:
		log.Fatalf("unknown -ask %q (want: stdin | allow | deny)", *askMode)
	}

	history := &History{}

	loop := &Loop{
		Provider:    p,
		Components:  bus,
		Strategy:    strategy,
		History:     history,
		Permissions: perms,
		Asker:       asker,
		MaxTurns:    *maxTurns,
		Verbose:     *verbose,
	}

	// Console UI on stderr keeps stdout clean for piping the final
	// answer (e.g. `s09-continuous-mode '...' | jq .`).
	ui := NewConsoleUI(os.Stderr)

	if *verbose {
		fmt.Fprintf(os.Stderr,
			"[s09-continuous-mode] provider=%s model=%s url=%s strategy=%s components=%d tools=%d directives=%d workspace=%s permissions=%s ask=%s cycles=%d ask-each-step=%v\n",
			*provider, model, url, *strategyFlag,
			len(bus.Components()), len(bus.Registry().All()), len(bus.Directives()),
			ws.Root(), sourceLabel, *askMode, *cycles, *askEachStep)
	}

	// Pass the prompt to the wrapper via the package-level binding;
	// see interaction_loop.go for the rationale.
	SetUserPrompt(prompt)

	final, err := RunInteractionLoop(context.Background(), loop, ui, LoopOpts{
		Cycles:      *cycles,
		AskEachStep: *askEachStep,
	})
	if err != nil {
		log.Fatalf("interaction loop error: %v", err)
	}
	if *verbose {
		fmt.Fprintf(os.Stderr, "[s09-continuous-mode] final history: %d episodes\n", len(*history))
	}
	if final != "" {
		fmt.Println(final)
	}
}

func loadPermissions(path string) (*Permissions, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return defaultPermissions(), "defaults", nil
		}
		return nil, "", fmt.Errorf("read %q: %w", path, err)
	}
	var cfg permissionsConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, "", fmt.Errorf("parse %q: %w", path, err)
	}
	p := NewPermissions()
	for _, glob := range cfg.Allow {
		p.AddAllow(glob)
	}
	for _, glob := range cfg.Deny {
		p.AddDeny(glob)
	}
	return p, "file:" + path, nil
}

func defaultPermissions() *Permissions {
	p := NewPermissions()
	p.AddAllow("read_file: **")
	p.AddAllow("write_file: **")
	p.AddAllow("web_fetch: **")
	return p
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
