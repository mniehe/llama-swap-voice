package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/mniehe/llama-swap-voice/internal/config"
	"github.com/mniehe/llama-swap-voice/internal/observability"
	"github.com/mniehe/llama-swap-voice/internal/server"
	"github.com/mniehe/llama-swap-voice/internal/upstream"
	"github.com/mniehe/llama-swap-voice/internal/voices"
)

const (
	serviceName       = "llama-swap-voice"
	envPrefix         = "LLAMA_SWAP_VOICE_"
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 60 * time.Second
	exitFailure       = 1
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", serviceName, err)
		os.Exit(exitFailure)
	}

	logger := observability.NewLogger(cfg.Logger.Level, cfg.Logger.Format)
	slog.SetDefault(logger)

	if err := run(&cfg, logger); err != nil {
		logger.Error("service stopped", slog.Any("error", err))
		os.Exit(exitFailure)
	}
}

func loadConfig() (ServiceConfig, error) {
	cfg, err := config.LoadConfig(envPrefix, defaultConfig)
	if err != nil {
		return cfg, fmt.Errorf("load configuration: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("invalid configuration:\n%w", err)
	}

	return cfg, nil
}

func run(cfg *ServiceConfig, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	voiceFiles, err := voices.Discover(cfg.Voices.Dir)
	if err != nil {
		return err
	}

	client, err := upstream.New(cfg.Upstream.URL, cfg.Upstream.TTSModel, cfg.Upstream.ASRModel, &http.Client{})
	if err != nil {
		return err
	}

	serverLogger := logger.With(slog.String(observability.ComponentLabel, "server"))
	serverCfg := cfg.serverConfig(voiceFiles)
	service, err := server.New(&serverCfg, client, serverLogger)
	if err != nil {
		return err
	}

	logger.Info("server starting",
		slog.String("name", serviceName),
		slog.String("version", Version),
		slog.String("host", cfg.Host),
		slog.Int("port", cfg.Port),
		slog.String("upstream", cfg.Upstream.URL),
		slog.Int("voices", len(voiceFiles)),
	)

	return serve(ctx, cfg, service, logger)
}

func serve(ctx context.Context, cfg *ServiceConfig, service *server.Server, logger *slog.Logger) error {
	ongoingCtx, stopOngoing := context.WithCancel(context.Background())
	defer stopOngoing()

	httpServer := &http.Server{
		Addr:              net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Handler:           service.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		BaseContext: func(net.Listener) context.Context {
			return ongoingCtx
		},
	}

	listenErr := make(chan error, 1)
	go func() {
		listenErr <- listen(httpServer)
	}()

	select {
	case <-ctx.Done():
	case err := <-listenErr:
		return err
	}

	logger.Info("received shutdown signal, shutting down")
	service.SetShuttingDown()
	time.Sleep(cfg.ShutdownTiming.Grace)

	shutdown(httpServer, &cfg.ShutdownTiming, logger)
	return nil
}

func listen(httpServer *http.Server) error {
	err := httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return fmt.Errorf("http server: %w", err)
}

// Request contexts descend from the server's BaseContext, which the caller
// cancels only after this returns, so in-flight audio streams are drained
// rather than aborted.
func shutdown(httpServer *http.Server, timing *ShutdownTimingConfig, logger *slog.Logger) {
	logger.Info("waiting for ongoing requests to finish", slog.String("timeout", timing.Total.String()))

	shutdownCtx, cancel := context.WithTimeout(context.Background(), timing.Total)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("timed out shutting down server gracefully",
			slog.Any("error", err),
			slog.String("hard_timeout", timing.Hard.String()),
		)
		time.Sleep(timing.Hard)
		return
	}

	logger.Info("server shut down")
}
