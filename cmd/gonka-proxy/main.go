package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/glaicer/gonka-proxy/internal/config"
	"github.com/glaicer/gonka-proxy/internal/proxy"
)

const shutdownTimeout = 10 * time.Second

func main() {
	configPath := flag.String("config", "config.yaml", "path to the YAML configuration file")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := log.New(os.Stdout, "", log.LstdFlags)
	if err := run(ctx, *configPath, logger); err != nil {
		logger.Printf("error=%v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, configPath string, logger proxy.Logger) error {
	if logger == nil {
		logger = log.New(os.Stdout, "", log.LstdFlags)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	handler, err := proxy.NewWithLogger(cfg, logger)
	if err != nil {
		return fmt.Errorf("create proxy: %w", err)
	}
	serverContext, cancelServerContext := context.WithCancel(ctx)
	defer cancelServerContext()
	requests := newRequestTracker()

	server := &http.Server{
		Addr:    cfg.ListenAddress,
		Handler: requests.wrap(handler),
		BaseContext: func(net.Listener) context.Context {
			return serverContext
		},
	}
	listener, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddress, err)
	}
	logger.Printf("listening on %s", cfg.ListenAddress)

	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.Serve(listener)
	}()

	select {
	case err := <-serveResult:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Printf("shutdown requested")
		requests.cancelAll()
		cancelServerContext()
		shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			_ = server.Close()
			return fmt.Errorf("shutdown: %w", err)
		}
		if err := <-serveResult; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve after shutdown: %w", err)
		}
		logger.Printf("shutdown complete")
		return nil
	}
}

type activeRequest struct {
	cancel context.CancelFunc
}

type requestTracker struct {
	mu       sync.Mutex
	active   map[*activeRequest]struct{}
	canceled bool
}

func newRequestTracker() *requestTracker {
	return &requestTracker{active: make(map[*activeRequest]struct{})}
}

func (t *requestTracker) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestContext, cancel := context.WithCancel(r.Context())
		request := &activeRequest{cancel: cancel}
		t.add(request)
		defer func() {
			t.remove(request)
			cancel()
		}()
		next.ServeHTTP(w, r.WithContext(requestContext))
	})
}

func (t *requestTracker) add(request *activeRequest) {
	t.mu.Lock()
	if t.canceled {
		t.mu.Unlock()
		request.cancel()
		return
	}
	t.active[request] = struct{}{}
	t.mu.Unlock()
}

func (t *requestTracker) remove(request *activeRequest) {
	t.mu.Lock()
	delete(t.active, request)
	t.mu.Unlock()
}

func (t *requestTracker) cancelAll() {
	t.mu.Lock()
	t.canceled = true
	active := make([]*activeRequest, 0, len(t.active))
	for request := range t.active {
		active = append(active, request)
	}
	t.mu.Unlock()

	for _, request := range active {
		request.cancel()
	}
}
