package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	claudeacp "github.com/savid/acp-go-claude"
	"github.com/savid/acp-go-claude/internal/claude"
)

var serve = claudeacp.Serve
var runMCPProxy = claudeacp.RunMCPProxy
var runClaudeCLI = runCLI
var exit = os.Exit
var shutdownOpenTelemetry = shutdownTelemetry
var agentVersion = buildVersion

const mcpProxyCommand = "mcp-proxy"
const cliCommandName = "acp-go-claude --cli"

func main() {
	if code := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); code != 0 {
		exit(code)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "--cli" {
		return runClaudeCLI(ctx, args[1:], stdin, stdout, stderr)
	}

	if len(args) > 0 && args[0] == mcpProxyCommand {
		return runProxy(ctx, args[1:], stdin, stdout, stderr)
	}

	flags := flag.NewFlagSet("acp-go-claude", flag.ContinueOnError)
	flags.SetOutput(stderr)

	claudePath := flags.String("claude", "", "path to claude CLI")
	model := flags.String("model", "", "default Claude model")
	claudeHome := flags.String("claude-home", "", "Claude config directory")
	bare := flags.Bool("bare", false, "launch Claude sessions with --bare; requires API-key or apiKeyHelper auth")
	hideClaudeAuth := flags.Bool("hide-claude-auth", false, "hide Claude subscription terminal auth methods")
	debug := flags.Bool("debug", false, "write debug logs to stderr")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	logger := slog.New(slog.DiscardHandler)
	if *debug {
		logger = slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	version := agentVersion()

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
		claudeacp.WithClaudePath(*claudePath),
		claudeacp.WithClaudeHome(*claudeHome),
		claudeacp.WithDefaultModel(*model),
		claudeacp.WithBareMode(*bare),
		claudeacp.WithHideClaudeAuth(*hideClaudeAuth),
		claudeacp.WithLogger(logger),
	)
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

func runCLI(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet(cliCommandName, flag.ContinueOnError)
	flags.SetOutput(stderr)

	claudePath := flags.String("claude", "", "path to claude CLI")
	claudeHome := flags.String("claude-home", "", "Claude config directory")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	path := *claudePath
	if path == "" {
		path = os.Getenv(claude.EnvClaudeCodeExecutable)
	}

	if path == "" {
		var err error

		path, err = exec.LookPath("claude")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: find claude in PATH: %v\n", cliCommandName, err)

			return 1
		}
	}

	cmd := exec.CommandContext(ctx, path, flags.Args()...) // #nosec G204,G702 -- path and args are the explicit Claude CLI auth command requested by the user.
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = os.Environ()

	if *claudeHome != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+*claudeHome)
	}

	if err := cmd.Start(); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", cliCommandName, err)

		return 1
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, forwardedSignals()...)

	done := make(chan struct{})

	go func() {
		defer signal.Stop(signals)

		for {
			select {
			case sig := <-signals:
				if cmd.Process != nil {
					_ = cmd.Process.Signal(sig)
				}
			case <-done:
				return
			}
		}
	}()

	err := cmd.Wait()

	close(done)

	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", cliCommandName, err)

		return commandExitCode(err)
	}

	return 0
}

func commandExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		if code >= 0 {
			return code
		}

		if code := signalExitCode(exitErr); code > 0 {
			return code
		}
	}

	return 1
}

func runProxy(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("acp-go-claude mcp-proxy", flag.ContinueOnError)
	flags.SetOutput(stderr)

	network := flags.String("network", "tcp", "bridge network")
	address := flags.String("address", "", "bridge address")
	acpID := flags.String("acp-id", "", "ACP MCP server id")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	tokenValue := readTokenFile(os.Getenv(claudeacp.MCPProxyTokenFileEnv))

	if *address == "" || tokenValue == "" || *acpID == "" {
		_, _ = fmt.Fprintf(
			stderr,
			"acp-go-claude mcp-proxy: -address, -acp-id, and %s are required\n",
			claudeacp.MCPProxyTokenFileEnv,
		)

		return 2
	}

	if err := runMCPProxy(ctx, stdin, stdout, claudeacp.MCPProxyOptions{
		Network: *network,
		Address: *address,
		Token:   tokenValue,
		ACPID:   *acpID,
	}); err != nil && ctx.Err() == nil {
		_, _ = fmt.Fprintf(stderr, "acp-go-claude mcp-proxy: %v\n", err)

		return 1
	}

	return 0
}

func readTokenFile(path string) string {
	if path == "" {
		return ""
	}

	data, err := os.ReadFile(path) // #nosec G304,G703 -- path is supplied by the parent agent through the proxy environment.
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(data))
}
