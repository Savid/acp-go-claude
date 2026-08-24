package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"

	claudeacp "github.com/savid/acp-go-claude"
)

// seedFileFlag collects repeatable -seed-file <relpath>=<hostpath> values,
// reading each host file's contents into a map keyed by the relative path.
type seedFileFlag struct {
	files map[string]string
}

func (s *seedFileFlag) String() string {
	if s == nil || len(s.files) == 0 {
		return ""
	}

	names := make([]string, 0, len(s.files))
	for name := range s.files {
		names = append(names, name)
	}

	slices.Sort(names)

	return strings.Join(names, ",")
}

func (s *seedFileFlag) Set(value string) error {
	relPath, hostPath, ok := strings.Cut(value, "=")
	if !ok {
		return fmt.Errorf("invalid -seed-file %q: expected <relpath>=<hostpath>", value)
	}

	relPath = strings.TrimSpace(relPath)
	hostPath = strings.TrimSpace(hostPath)

	if relPath == "" || hostPath == "" {
		return fmt.Errorf("invalid -seed-file %q: expected <relpath>=<hostpath>", value)
	}

	contents, err := os.ReadFile(hostPath)
	if err != nil {
		return fmt.Errorf("read seed file %q: %w", hostPath, err)
	}

	if s.files == nil {
		s.files = make(map[string]string)
	}

	s.files[relPath] = string(contents)

	return nil
}

var serve = claudeacp.Serve
var exit = os.Exit
var shutdownOpenTelemetry = shutdownTelemetry
var agentVersion = version

func main() {
	if code := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); code != 0 {
		exit(code)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "containment" {
		return runContainment(args[1:], stdout, stderr)
	}

	flags := flag.NewFlagSet("acp-go-claude", flag.ContinueOnError)
	flags.SetOutput(stderr)

	claudePath := flags.String("path", "", "path to claude CLI")
	claudeHome := flags.String("home", "", "Claude config directory")
	scratchDir := flags.String("scratch-dir", "", "parent directory for ephemeral session scratch; empty means the system temp directory")
	providerAuthRoot := flags.String("provider-auth-root", "", "durable host-owned root holding the provider-auth ledger; empty, static auth, or bare mode leaves the surface unadvertised")
	providerAuthDirectHome := flags.String("provider-auth-direct-home", "", "exact Claude config directory a native-login disconnect may clear")
	isolationConfigPath := flags.String(processIsolationConfigFlag, "", "absolute path to an optional root-owned mode-0600 Linux child-isolation policy; omitted runs native work as the current identity")
	model := flags.String("model", "", "default Claude model")
	bare := flags.Bool("claude-bare", false, "launch Claude sessions with --bare; requires API-key or apiKeyHelper auth")
	permissionMode := flags.String("claude-permission-mode", "", "default Claude permission mode")
	systemPrompt := flags.String("claude-system-prompt", "", "default Claude system prompt")
	hideClaudeAuth := flags.Bool("claude-hide-auth", false,
		"withdraw the Claude subscription sign-in from the advertised auth catalog")
	seedFiles := &seedFileFlag{}
	flags.Var(seedFiles, "seed-file", "seed file written into the Claude config dir as <relpath>=<hostpath>; repeatable")
	settingsFile := flags.String("claude-settings-file", "", "settings overlay relpath under the Claude config dir passed as --settings; requires -home")
	debug := flags.Bool("debug", false, "write debug logs to stderr")
	printVersion := flags.Bool("version", false, "print adapter version and exit")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	version := agentVersion()

	if *printVersion {
		_, _ = fmt.Fprintln(stdout, version)

		return 0
	}

	// An explicit policy path is strict hardening: it loads and validates
	// before anything serves, and any failure refuses startup. Omission appends
	// no isolation option at all, so native work runs as the current identity
	// and -home keeps its ordinary managed-home meaning.
	var isolationOptions []claudeacp.Option

	if *isolationConfigPath != "" {
		isolation, err := processIsolationConfigLoader(*isolationConfigPath)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "acp-go-claude: process isolation: %v\n", err)

			return 1
		}

		if *claudeHome == "" {
			*claudeHome = isolation.StandaloneStateRoot
		}

		if !filepath.IsAbs(*claudeHome) || filepath.Clean(*claudeHome) != *claudeHome ||
			*claudeHome != isolation.StandaloneStateRoot {
			_, _ = fmt.Fprintf(stderr, "acp-go-claude: -home must equal standaloneStateRoot %q\n", isolation.StandaloneStateRoot)

			return 1
		}

		isolationOptions = append(isolationOptions, claudeacp.WithProcessIsolation(claudeacp.ProcessIsolation{
			UID:                 isolation.UID,
			GID:                 isolation.GID,
			BaseEnvironment:     isolation.BaseEnvironment,
			StandaloneOwnerID:   isolation.StandaloneOwnerID,
			StandaloneStateRoot: isolation.StandaloneStateRoot,
		}))
	}

	logger := slog.New(slog.DiscardHandler)
	if *debug {
		logger = slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	telemetry, err := configureTelemetry(ctx, logger, version)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "acp-go-claude: configure OpenTelemetry: %v\n", err)

		return 1
	}

	logger = telemetry.logger

	signals := forwardedSignals()
	receivedSignals := make(chan os.Signal, 1)

	// NotifyContext cancels serving on a signal; this channel preserves the
	// actual signal value so the process can return the conventional exit code.
	signal.Notify(receivedSignals, signals...)
	defer signal.Stop(receivedSignals)

	ctx, stop := signal.NotifyContext(ctx, signals...)
	defer stop()

	serveOptions := make([]claudeacp.Option, 0, 6+len(telemetry.options))

	serveOptions = append(serveOptions,
		claudeacp.WithAgentVersion(version),
		claudeacp.WithExecutablePath(*claudePath),
		claudeacp.WithHome(*claudeHome),
		claudeacp.WithScratchDir(*scratchDir),
		claudeacp.WithProviderAuthRoot(*providerAuthRoot),
		claudeacp.WithProviderAuthDirectHome(*providerAuthDirectHome),
		claudeacp.WithDefaultModel(*model),
		claudeacp.WithClaudeBareMode(*bare),
		claudeacp.WithClaudeHideAuth(*hideClaudeAuth),
		claudeacp.WithLogger(logger),
	)

	serveOptions = append(serveOptions, isolationOptions...)
	if *permissionMode != "" {
		serveOptions = append(serveOptions, claudeacp.WithClaudeDefaultPermissionMode(*permissionMode))
	}

	if *systemPrompt != "" {
		serveOptions = append(serveOptions, claudeacp.WithClaudeDefaultSystemPrompt(*systemPrompt))
	}

	if len(seedFiles.files) > 0 {
		serveOptions = append(serveOptions, claudeacp.WithSeedFiles(seedFiles.files))
	}

	if *settingsFile != "" {
		serveOptions = append(serveOptions, claudeacp.WithClaudeSettingsFile(*settingsFile))
	}

	serveOptions = append(serveOptions, telemetry.options...)

	serveErr := serve(ctx, stdin, stdout, serveOptions...)
	shutdownErr := shutdownOpenTelemetry(context.Background(), telemetry.shutdown)

	if serveErr != nil && ctx.Err() == nil {
		_, _ = fmt.Fprintf(stderr, "acp-go-claude: %v\n", serveErr)

		return 1
	}

	if shutdownErr != nil {
		_, _ = fmt.Fprintf(stderr, "acp-go-claude: shutdown OpenTelemetry: %v\n", shutdownErr)

		return 1
	}

	if sig := pendingSignal(receivedSignals); sig != nil {
		return signalCode(sig)
	}

	return 0
}

func pendingSignal(signals <-chan os.Signal) os.Signal {
	select {
	case sig := <-signals:
		return sig
	default:
		return nil
	}
}
