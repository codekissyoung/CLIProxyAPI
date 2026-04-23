package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/profilerwatch"
	log "github.com/sirupsen/logrus"
)

var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

func init() {
	logging.SetupBaseLogger()
	buildinfo.Version = Version
	buildinfo.Commit = Commit
	buildinfo.BuildDate = BuildDate
}

func main() {
	if err := run(); err != nil {
		log.Errorf("cliproxy profiler watcher failed: %v", err)
		os.Exit(1)
	}
}

func run() error {
	var configPath string
	var checkOnly bool
	var showVersion bool

	flag.StringVar(&configPath, "config", "./examples/cliproxy-profiler/cliproxy-profiler.example.yaml", "Path to cliproxy profiler watcher config YAML")
	flag.BoolVar(&checkOnly, "check", false, "Validate config, resolve runtime targets, and probe pprof once")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("cliproxy-profiler version=%s commit=%s built=%s\n", buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate)
		return nil
	}

	cfg, err := profilerwatch.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load profiler watcher config: %w", err)
	}

	level, err := log.ParseLevel(cfg.LogLevel)
	if err != nil {
		return fmt.Errorf("invalid log level %q: %w", cfg.LogLevel, err)
	}
	log.SetLevel(level)

	watcher, err := profilerwatch.New(cfg)
	if err != nil {
		return fmt.Errorf("construct watcher: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if checkOnly {
		return watcher.Check(ctx)
	}

	log.WithFields(log.Fields{
		"version": buildinfo.Version,
		"commit":  buildinfo.Commit,
		"config":  configPath,
	}).Info("starting cliproxy profiler watcher")

	if err := watcher.Run(ctx); err != nil {
		return err
	}
	log.Info("cliproxy profiler watcher stopped")
	return nil
}
