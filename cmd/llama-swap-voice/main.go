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
	"github.com/mniehe/llama-swap-voice/internal/openai"
	"github.com/mniehe/llama-swap-voice/internal/pipeline"
	"github.com/mniehe/llama-swap-voice/internal/recordings"
	"github.com/mniehe/llama-swap-voice/internal/upstream"
	"github.com/mniehe/llama-swap-voice/internal/voices"
	"github.com/mniehe/llama-swap-voice/internal/wyoming"
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

	client, err := upstream.New(cfg.Upstream.URL, cfg.Upstream.TTSModel, cfg.Upstream.ASRModel, &http.Client{}, upstream.WithBackend(cfg.Upstream.TTSBackend))
	if err != nil {
		return err
	}

	service, err := newServices(cfg, voiceFiles, client, logger)
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

// services pairs the HTTP entrypoint with the pipeline whose admission the
// shutdown drains, and the recordings store when recordings are on.
type services struct {
	api        *openai.Server
	pipeline   *pipeline.Pipeline
	recordings *recordings.Store
	// runRecordings is the store's background work, nil when recordings are
	// off; a field so that a test can watch serve start and stop it.
	runRecordings func(ctx, drain context.Context)
}

func newServices(cfg *ServiceConfig, voiceFiles map[string]string, client *upstream.Client, logger *slog.Logger) (*services, error) {
	store, err := openRecordings(&cfg.Recordings, logger.With(slog.String(observability.ComponentLabel, "recordings")))
	if err != nil {
		return nil, err
	}

	serverLogger := logger.With(slog.String(observability.ComponentLabel, "server"))
	serverCfg := cfg.serverConfig(voiceFiles)
	var options []openai.Option
	if store != nil {
		serverCfg.Recorder = store
		options = append(options, openai.WithRecordings(store))
	}

	pipe, err := pipeline.New(&serverCfg, client, serverLogger)
	if err != nil {
		return nil, err
	}

	service := &services{api: openai.New(pipe, serverLogger, options...), pipeline: pipe, recordings: store}
	if store != nil {
		service.runRecordings = store.Run
	}

	return service, nil
}

// openRecordings returns nil when recordings are off.
func openRecordings(cfg *RecordingsConfig, logger *slog.Logger) (*recordings.Store, error) {
	if !cfg.Enable {
		return nil, nil
	}

	store, err := recordings.Open(cfg.Dir, time.Now, logger)
	if err != nil {
		return nil, fmt.Errorf("recordings in %s: %w", cfg.Dir, err)
	}
	logger.Info("recordings enabled", slog.String("dir", cfg.Dir))

	return store, nil
}

// startRecordings runs the recordings store's background work. The returned
// stop ends run's ctx at once and its drain at deadline, and waits for run
// to return until deadline: a save under way when the deadline passes is
// left to finish or die with the process.
func startRecordings(run func(ctx, drain context.Context), logger *slog.Logger) (stop func(deadline time.Time)) {
	if run == nil {
		return func(time.Time) {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	drain, endDrain := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx, drain)
	}()

	return func(deadline time.Time) {
		cancel()
		defer endDrain()

		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()

		select {
		case <-done:
		case <-timer.C:
			endDrain()
			logger.Warn("recordings still saving at the shutdown deadline")
		}
	}
}

// recordingsDeadline is when the recordings worker must stop: the shutdown
// budget, timing.Total and timing.Hard, counted from the start of shutdown,
// or from now when shutdown has not started.
func recordingsDeadline(start time.Time, timing *ShutdownTimingConfig) time.Time {
	if start.IsZero() {
		start = time.Now()
	}

	return start.Add(timing.Total + timing.Hard)
}

// entrypoints are the two listeners that shutdown stops together. The
// Wyoming server has no listener when it is disabled; its Shutdown and Close
// then do nothing.
type entrypoints struct {
	http    *http.Server
	wyoming *wyoming.Server
}

// listeners are the bound sockets serveOn serves. wyoming is nil when the
// Wyoming listener is disabled.
type listeners struct {
	http    net.Listener
	wyoming net.Listener
}

type listenFunc func(ctx context.Context, network, addr string) (net.Listener, error)

func serve(ctx context.Context, cfg *ServiceConfig, service *services, logger *slog.Logger) error {
	return serveWith(ctx, cfg, service, logger, (&net.ListenConfig{}).Listen)
}

func serveWith(ctx context.Context, cfg *ServiceConfig, service *services, logger *slog.Logger, listen listenFunc) error {
	ls, err := bind(ctx, cfg, logger, listen)
	if err != nil {
		return err
	}

	return serveOn(ctx, cfg, service, ls, logger)
}

// bind opens the Wyoming listener before the HTTP one, so the readiness
// probe never answers while a bind can still fail. When the HTTP bind fails
// it closes the Wyoming listener before it returns.
func bind(ctx context.Context, cfg *ServiceConfig, logger *slog.Logger, listen listenFunc) (*listeners, error) {
	ls := &listeners{}
	if cfg.Wyoming.Enabled {
		listener, err := listen(ctx, "tcp", net.JoinHostPort(cfg.Wyoming.Host, strconv.Itoa(cfg.Wyoming.Port)))
		if err != nil {
			return nil, fmt.Errorf("wyoming listener: %w", err)
		}
		logger.Info("wyoming listening", slog.String("address", listener.Addr().String()))
		ls.wyoming = listener
	}

	// A stop signal while the HTTP address resolves must end in the grace
	// period, not in a failed start.
	listener, err := listen(context.WithoutCancel(ctx), "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		err = fmt.Errorf("http listener: %w", err)
		if ls.wyoming == nil {
			return nil, err
		}
		return nil, errors.Join(err, ls.wyoming.Close())
	}
	ls.http = listener

	return ls, nil
}

// serveOn owns the listeners in ls and closes them before it returns.
func serveOn(ctx context.Context, cfg *ServiceConfig, service *services, ls *listeners, logger *slog.Logger) error {
	defer closeListeners(ls, logger)
	stopRecordings := startRecordings(service.runRecordings, logger)
	var shutdownStart time.Time
	defer func() { stopRecordings(recordingsDeadline(shutdownStart, &cfg.ShutdownTiming)) }()
	ongoingCtx, stopOngoing := context.WithCancel(context.Background())
	defer stopOngoing()

	wyomingCfg := cfg.wyomingConfig(Version)
	servers := &entrypoints{
		http: &http.Server{
			Handler:           service.api.Handler(),
			ReadHeaderTimeout: readHeaderTimeout,
			IdleTimeout:       idleTimeout,
			BaseContext: func(net.Listener) context.Context {
				return ongoingCtx
			},
		},
		wyoming: wyoming.New(ongoingCtx, &wyomingCfg, service.pipeline, logger.With(slog.String(observability.ComponentLabel, "wyoming"))),
	}

	listenErr := make(chan error, 2)
	if ls.wyoming != nil {
		go func() {
			listenErr <- serveWyoming(servers.wyoming, ls.wyoming)
		}()
	}
	go func() {
		listenErr <- serveHTTP(servers.http, ls.http)
	}()

	select {
	case <-ctx.Done():
	case err := <-listenErr:
		shutdownStart = time.Now()
		return errors.Join(err, shutdown(servers, service, stopOngoing, &cfg.ShutdownTiming, logger))
	}

	logger.Info("received shutdown signal, shutting down")
	service.api.SetShuttingDown()
	time.Sleep(cfg.ShutdownTiming.Grace)

	shutdownStart = time.Now()
	return shutdown(servers, service, stopOngoing, &cfg.ShutdownTiming, logger)
}

// closeListeners is needed because shutdown closes only the listeners a
// server has registered, and a server registers one only once its goroutine
// has entered Serve, which a signal or the other server's failure can beat.
func closeListeners(ls *listeners, logger *slog.Logger) {
	for _, listener := range []net.Listener{ls.http, ls.wyoming} {
		if listener == nil {
			continue
		}
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			logger.Warn("closing listener", slog.String("address", listener.Addr().String()), slog.Any("error", err))
		}
	}
}

func serveHTTP(httpServer *http.Server, listener net.Listener) error {
	err := httpServer.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return fmt.Errorf("http server: %w", err)
}

func serveWyoming(server *wyoming.Server, listener net.Listener) error {
	if err := server.Serve(listener); err != nil {
		return fmt.Errorf("wyoming server: %w", err)
	}

	return nil
}

// Request contexts descend from ongoingCtx, so in-flight requests on both
// listeners first get timing.Total to finish untouched. Past that they are
// cancelled and connections are closed; timing.Hard then bounds the wait for
// their handlers and the Wyoming connection goroutines to return.
func shutdown(
	servers *entrypoints,
	service *services,
	cancelRequests context.CancelFunc,
	timing *ShutdownTimingConfig,
	logger *slog.Logger,
) error {
	logger.Info("waiting for ongoing requests to finish", slog.String("timeout", timing.Total.String()))

	err := drain(servers, timing.Total)
	if err == nil {
		logger.Info("server shut down")
		return nil
	}

	logger.Warn("requests outlived the shutdown budget, cancelling them",
		slog.Any("error", err),
		slog.String("hard_timeout", timing.Hard.String()),
	)
	cancelRequests()
	if err := servers.http.Close(); err != nil {
		logger.Warn("closing connections", slog.Any("error", err))
	}
	if err := servers.wyoming.Close(); err != nil {
		logger.Warn("closing wyoming connections", slog.Any("error", err))
	}

	return awaitHandlers(service.pipeline, servers.wyoming, timing.Hard)
}

// drain shuts both listeners down at once under one budget; it succeeds only
// when both do.
func drain(servers *entrypoints, total time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), total)
	defer cancel()

	wyomingErr := make(chan error, 1)
	go func() {
		wyomingErr <- servers.wyoming.Shutdown(ctx)
	}()

	var errs []error
	if err := servers.http.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("http server: %w", err))
	}
	if err := <-wyomingErr; err != nil {
		errs = append(errs, fmt.Errorf("wyoming server: %w", err))
	}

	return errors.Join(errs...)
}

// awaitHandlers waits for the pipeline's handlers and, through a second
// Shutdown after Close, for the Wyoming connection goroutines.
func awaitHandlers(pipe *pipeline.Pipeline, wyomingServer *wyoming.Server, hard time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), hard)
	defer cancel()

	if err := errors.Join(pipe.Drain(ctx), wyomingServer.Shutdown(ctx)); err != nil {
		return fmt.Errorf("requests still running %s after cancellation: %w", hard, err)
	}

	return nil
}
