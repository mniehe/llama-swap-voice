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

	return shutdown(httpServer, service, stopOngoing, &cfg.ShutdownTiming, logger)
}

func listen(httpServer *http.Server) error {
	err := httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return fmt.Errorf("http server: %w", err)
}

// Request contexts descend from the server's BaseContext, so in-flight audio
// streams first get timing.Total to finish untouched. Past that they are
// cancelled, which signals their ffmpeg children, and connections are closed;
// timing.Hard then bounds the wait for handlers to reap those children.
func shutdown(
	httpServer *http.Server,
	service *server.Server,
	cancelRequests context.CancelFunc,
	timing *ShutdownTimingConfig,
	logger *slog.Logger,
) error {
	logger.Info("waiting for ongoing requests to finish", slog.String("timeout", timing.Total.String()))

	err := drain(httpServer, timing.Total)
	if err == nil {
		logger.Info("server shut down")
		return nil
	}

	logger.Warn("requests outlived the shutdown budget, cancelling them",
		slog.Any("error", err),
		slog.String("hard_timeout", timing.Hard.String()),
	)
	cancelRequests()
	if err := httpServer.Close(); err != nil {
		logger.Warn("closing connections", slog.Any("error", err))
	}

	return awaitHandlers(service, timing.Hard)
}

func drain(httpServer *http.Server, total time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), total)
	defer cancel()

	return httpServer.Shutdown(ctx)
}

func awaitHandlers(service *server.Server, hard time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), hard)
	defer cancel()

	if err := service.Drain(ctx); err != nil {
		return fmt.Errorf("requests still running %s after cancellation: %w", hard, err)
	}

	return nil
}
