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
)

// Provider profiles — same one-liner shortcuts every later session
// inherits. s07 keeps the surface unchanged. The new thing in s07 is
// internal: a `Permissions` rule set is loaded from
// `permissions.json` (if present) or seeded with sensible defaults
// (read_file/write_file/echo/math all permitted), and the Loop now has
// a permission gate between Parse and Execute.
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

// permissionsConfig is the on-disk shape we (un)marshal. JSON-only —
// no YAML dep — so the binary stays stdlib-only. Field names are
// lower-case to match the documented config style; `allow` and `deny`
// are arrays of pattern strings ("<cmd>: <arg-glob>" each).
type permissionsConfig struct {
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

func main() {
	verbose := flag.Bool("v", false, "print every turn (assistant text + tool calls + permission decisions)")
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
		"workspace root directory (auto-mkdir if absent)")
	permsPath := flag.String("permissions", envOr("PERMISSIONS", "./permissions.json"),
		"path to permissions.json (allow/deny rule list); falls back to built-in defaults if missing")
	askMode := flag.String("ask", envOr("ASK", "deny"),
		"behavior on Ask decisions: stdin (interactive y/N), allow (auto-allow), deny (auto-deny). "+
			"Default 'deny' is fail-closed for safety.")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: s07-permissions [-v] [-provider P] [-strategy S] [-workspace DIR] [-permissions FILE] [-ask MODE] <prompt>\n\n"+
				"  Permission rules:\n"+
				"    -permissions FILE  JSON file with {\"allow\": [...], \"deny\": [...]}\n"+
				"    Pattern format     \"<command>: <arg-glob>\" — e.g. \"read_file: *.md\", \"bash: rm -rf*\"\n"+
				"    Defaults (when FILE missing): allow read_file/write_file/echo/math; no deny rules.\n\n"+
				"  -ask deny    (default) any Ask-decision is auto-denied; the agent sees a denial result.\n"+
				"  -ask allow   any Ask-decision is auto-allowed (DANGEROUS — no human in the loop).\n"+
				"  -ask stdin   prompts y/N on stderr per Ask decision.\n\n"+
				"  Provider profiles, strategies, and workspace flags: see s06.\n\n"+
				"  Example:\n"+
				"    s07-permissions -v -ask stdin \"create notes.md with a one-line summary\"\n")
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

	// Permissions: load from file if present, else fall back to defaults.
	perms, sourceLabel, err := loadPermissions(*permsPath)
	if err != nil {
		log.Fatalf("load permissions: %v", err)
	}

	// Asker: chosen via -ask flag.
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
		Tools:       reg,
		Strategy:    strategy,
		History:     history,
		Permissions: perms,
		Asker:       asker,
		MaxTurns:    *maxTurns,
		Verbose:     *verbose,
	}
	if *verbose {
		fmt.Fprintf(os.Stderr,
			"[s07-permissions] provider=%s model=%s url=%s strategy=%s tools=%d workspace=%s permissions=%s ask=%s\n",
			*provider, model, url, *strategyFlag, len(reg.All()), ws.Root(), sourceLabel, *askMode)
	}

	final, err := loop.Run(context.Background(), prompt)
	if err != nil {
		log.Fatalf("loop error: %v", err)
	}
	if *verbose {
		fmt.Fprintf(os.Stderr, "[s07-permissions] final history: %d episodes\n", len(*history))
	}
	fmt.Println(final)
}

// loadPermissions reads `path` as JSON {"allow":[],"deny":[]}. If the
// file is missing, returns a default rule set covering the four
// builtin tools. The second return is a human-readable label for the
// verbose banner ("file:./permissions.json" or "defaults").
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

// defaultPermissions: allow the four builtin tools without args
// inspection, no deny rules. This keeps the s07 quickstart working
// without requiring a config file — but the user is encouraged to
// drop in a real `permissions.json` once they understand the model.
func defaultPermissions() *Permissions {
	p := NewPermissions()
	p.AddAllow("read_file: **")
	p.AddAllow("write_file: **")
	p.AddAllow("echo: **")
	p.AddAllow("math: **")
	return p
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
