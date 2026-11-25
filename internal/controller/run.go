package controller

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/letthemplay/Strata/internal/agent"
	"github.com/letthemplay/Strata/internal/agent/api"
	"github.com/letthemplay/Strata/internal/agent/client"
	"github.com/letthemplay/Strata/internal/cli"
	"github.com/letthemplay/Strata/internal/gitops"
)

// runCommand is `strata-controller run`, the service.
func runCommand() cli.Subcommand {
	return cli.Subcommand{
		Name:    "run",
		Usage:   "--config FILE --state-dir DIR [--credentials DIR] [--agent-socket PATH] [--api-group GROUP]",
		Summary: "Run the controller (systemd service; the API socket comes from socket activation)",
		Run: func(ctx context.Context, args []string, _, stderr io.Writer) int {
			fset := flag.NewFlagSet("run", flag.ContinueOnError)
			fset.SetOutput(stderr)
			configFile := fset.String("config", "", "configuration (JSON, written by strata.gitops)")
			stateDir := fset.String("state-dir", "/var/lib/strata-controller", "state directory")
			creds := fset.String("credentials", "", "credentials directory (systemd LoadCredential)")
			agentSocket := fset.String("agent-socket", client.DefaultSocket, "the agent's API socket")
			apiGroup := fset.String("api-group", "strata-operators", "group whose members may use the controller's API")
			evalSocket := fset.String("eval-socket", DefaultEvalSocket, "the evaluation service's socket")
			if err := fset.Parse(args); err != nil {
				if errors.Is(err, flag.ErrHelp) {
					return cli.ExitOK
				}
				return cli.ExitUsage
			}
			if *configFile == "" || fset.NArg() != 0 {
				_, _ = fmt.Fprintln(stderr, "run: --config is required and no arguments are accepted")
				return cli.ExitUsage
			}
			logger := slog.New(slog.NewJSONHandler(stderr, nil))
			if err := serve(ctx, logger, *configFile, *stateDir, *creds, *agentSocket, *apiGroup, *evalSocket); err != nil {
				logger.ErrorContext(ctx, "controller failed", "error", err)
				return cli.ExitError
			}
			return cli.ExitOK
		},
	}
}

func serve(ctx context.Context, logger *slog.Logger, configFile, stateDir, creds, agentSocket, apiGroup, evalSocket string) error {
	cfg, err := LoadConfig(configFile)
	if err != nil {
		return err
	}
	signers, err := gitops.AllowedSigners(cfg.TrustedSigners)
	if err != nil {
		return fmt.Errorf("trusted signers: %w", err)
	}
	allowed := filepath.Join(stateDir, "allowed_signers")
	if err := os.WriteFile(allowed, []byte(signers), 0o600); err != nil {
		return fmt.Errorf("write allowed signers: %w", err)
	}
	repo := &gitops.Repo{
		Dir: filepath.Join(stateDir, "repo.git"), URL: cfg.Repository, Branch: cfg.Branch,
		KnownHostsFile: cfg.KnownHostsFile, AllowedSignersFile: allowed,
	}
	if cfg.SSHKey != "" {
		repo.SSHKeyFile = filepath.Join(creds, cfg.SSHKey)
	}
	if cfg.Token != "" {
		repo.TokenFile = filepath.Join(creds, cfg.Token)
	}
	if err := repo.Check(); err != nil {
		return err //nolint:wrapcheck // The message names the setting.
	}
	c := &Controller{
		Config: cfg, Repo: repo, Log: logger,
		Eval: &RemoteEvaluator{Socket: evalSocket, Timeout: time.Duration(cfg.EvalTimeoutSeconds) * time.Second},
		Node: &client.Client{Socket: agentSocket},
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	listener, err := agent.SystemdListener()
	if err != nil {
		return err //nolint:wrapcheck // Names the socket.
	}
	errc := make(chan error, 1)
	if listener != nil {
		srv := &Server{Controller: c, Authorize: api.GroupAuthorizer(apiGroup), Log: logger}
		go func() { errc <- srv.Serve(ctx, listener) }()
	} else {
		logger.WarnContext(ctx, "no socket from systemd: the controller's API is not served")
	}
	logger.InfoContext(ctx, "controller started", "node", cfg.Node, "repository", cfg.Repository, "branch", cfg.Branch,
		"interval", cfg.Interval().String(), "autoApply", cfg.AutoApply)
	c.Run(ctx)
	if listener != nil {
		return <-errc
	}
	return nil
}

// DefaultEvalSocket is the evaluation service's socket.
const DefaultEvalSocket = "/run/strata/controller-eval.sock"

// evalCommand is `strata-controller eval-service`: one evaluation per
// connection, on standard input and output (systemd socket activation
// with Accept=yes). The service has no network (ADR-0025).
func evalCommand(stdin io.Reader, stdout io.Writer) cli.Subcommand {
	return cli.Subcommand{
		Name:    "eval-service",
		Usage:   "--state-dir DIR --cache-dir DIR --timeout SECONDS",
		Summary: "Evaluate one commit's IR for the controller (systemd service, one per connection)",
		Run: func(ctx context.Context, args []string, _, stderr io.Writer) int {
			fset := flag.NewFlagSet("eval-service", flag.ContinueOnError)
			fset.SetOutput(stderr)
			stateDir := fset.String("state-dir", "/var/lib/strata-controller", "the controller's state directory")
			cacheDir := fset.String("cache-dir", "/var/cache/strata-controller-eval", "Nix's evaluation cache")
			timeout := fset.Int("timeout", 300, "seconds an evaluation may take")
			if err := fset.Parse(args); err != nil || fset.NArg() != 0 {
				return cli.ExitUsage
			}
			e := &Evaluator{Home: *cacheDir, Timeout: time.Duration(*timeout) * time.Second}
			rw := struct {
				io.Reader
				io.Writer
			}{stdin, stdout}
			if err := ServeEval(ctx, rw, e, filepath.Join(*stateDir, "repo.git")); err != nil {
				_, _ = fmt.Fprintf(stderr, "eval-service: %v\n", err)
				return cli.ExitError
			}
			return cli.ExitOK
		},
	}
}
