package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"

	claudeacp "github.com/savid/acp-go-claude"
)

var serve = claudeacp.Serve
var exit = os.Exit
var shutdownOpenTelemetry = shutdownTelemetry
var agentVersion = buildVersion

func main() {
	if code := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); code != 0 {
		exit(code)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("acp-go-claude", flag.ContinueOnError)
	flags.SetOutput(stderr)

	claudePath := flags.String("path", "", "path to claude CLI")
	claudeHome := flags.String("home", "", "Claude config directory")
	model := flags.String("model", "", "default Claude model")
	bare := flags.Bool("claude-bare", false, "launch Claude sessions with --bare; requires API-key or apiKeyHelper auth")
	permissionMode := flags.String("claude-permission-mode", "", "default Claude permission mode")
	systemPrompt := flags.String("claude-system-prompt", "", "default Claude system prompt")
	hideClaudeAuth := flags.Bool("claude-hide-auth", false, "hide Claude subscription terminal auth methods")
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

	serveOptions := make([]claudeacp.Option, 0, 5+len(telemetry.options))

	serveOptions = append(serveOptions,
		claudeacp.WithAgentVersion(version),
		claudeacp.WithExecutablePath(*claudePath),
		claudeacp.WithHome(*claudeHome),
		claudeacp.WithDefaultModel(*model),
		claudeacp.WithClaudeBareMode(*bare),
		claudeacp.WithClaudeHideAuth(*hideClaudeAuth),
		claudeacp.WithLogger(logger),
	)
	if *permissionMode != "" {
		serveOptions = append(serveOptions, claudeacp.WithClaudeDefaultPermissionMode(*permissionMode))
	}

	if *systemPrompt != "" {
		serveOptions = append(serveOptions, claudeacp.WithClaudeDefaultSystemPrompt(*systemPrompt))
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
