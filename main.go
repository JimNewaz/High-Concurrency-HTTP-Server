package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

// Config holds runtime configuration options.
type Config struct {
	Port           string
	MaxConcurrent  int
	QueueTimeout   time.Duration
	ShutdownGrace  time.Duration
	ProcessingTime time.Duration
}

// Semaphore implements backpressure / concurrency control using a buffered channel.
type Semaphore struct {
	sem          chan struct{}
	queueTimeout time.Duration
	inFlight     atomic.Int64
	rejected     atomic.Uint64
}

// NewSemaphore initializes a bounded concurrency limiter.
func NewSemaphore(maxConcurrent int, queueTimeout time.Duration) *Semaphore {
	return &Semaphore{
		sem:          make(chan struct{}, maxConcurrent),
		queueTimeout: queueTimeout,
	}
}

// LimitMiddleware sheds load or queues requests when max concurrency is saturated.
func (s *Semaphore) LimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var timer *time.Timer
		var timeoutCh <-chan time.Time

		if s.queueTimeout > 0 {
			timer = time.NewTimer(s.queueTimeout)
			defer timer.Stop()
			timeoutCh = timer.C
		}

		select {
		case s.sem <- struct{}{}:
			// Acquired execution slot
			s.inFlight.Add(1)
			defer func() {
				s.inFlight.Add(-1)
				<-s.sem
			}()
			next.ServeHTTP(w, r)

		case <-timeoutCh:
			// Backpressure triggered: queue acquisition timed out
			s.rejected.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":   "Service Unavailable",
				"message": "Server backpressure limit reached. Please retry.",
			})

		case <-r.Context().Done():
			// Client disconnected while waiting in queue
			return
		}
	})
}

type Response struct {
	Status    string `json:"status"`
	Timestamp int64  `json:"timestamp"`
	InFlight  int64  `json:"in_flight"`
}

func main() {
	// 1. Structured JSON Logger
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// 2. Load Configuration (defaults optimized for 5,000 concurrent connection test)
	cfg := Config{
		Port:           getEnv("PORT", "8080"),
		MaxConcurrent:  getEnvInt("MAX_CONCURRENT", 5000),
		QueueTimeout:   getEnvDuration("QUEUE_TIMEOUT_MS", 200*time.Millisecond),
		ShutdownGrace:  getEnvDuration("SHUTDOWN_GRACE_SEC", 15*time.Second),
		ProcessingTime: 50 * time.Millisecond,
	}

	// 3. Setup Backpressure Semaphore
	limiter := NewSemaphore(cfg.MaxConcurrent, cfg.QueueTimeout)

	// 4. HTTP Routes
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		// Simulate 50ms processing delay, respecting request context cancellation
		timer := time.NewTimer(cfg.ProcessingTime)
		defer timer.Stop()

		select {
		case <-timer.C:
			// Simulated work complete
		case <-r.Context().Done():
			// Request cancelled by client before work finished
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(Response{
			Status:    "ok",
			Timestamp: time.Now().UnixMilli(),
			InFlight:  limiter.inFlight.Load(),
		})
	})

	// Wrap root router with our concurrency limiter middleware
	handler := limiter.LimitMiddleware(mux)

	// 5. Production-grade HTTP Server with explicit connection timeouts
	server := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: handler,

		// ReadHeaderTimeout prevents Slowloris attacks by capping header read time
		ReadHeaderTimeout: 2 * time.Second,

		// ReadTimeout covers the time from connection accept to request body read completion
		ReadTimeout: 5 * time.Second,

		// WriteTimeout covers the time from end of request header read to end of response write
		WriteTimeout: 10 * time.Second,

		// IdleTimeout defines the maximum amount of time to wait for the next request when keep-alives are enabled
		IdleTimeout: 120 * time.Second,

		// MaxHeaderBytes mitigates large header buffer allocation attacks
		MaxHeaderBytes: 1 << 20, // 1 MB
	}

	// 6. Graceful Shutdown Signal Interception
	serverCtx, stopServer := context.WithCancel(context.Background())
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		slog.Info("Termination signal received. Initiating graceful shutdown...",
			"signal", sig.String(),
			"grace_period", cfg.ShutdownGrace.String(),
			"current_in_flight", limiter.inFlight.Load(),
		)

		// Context with timeout to give active in-flight requests time to complete
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("Graceful shutdown failed; forcing server termination", "error", err)
		} else {
			slog.Info("All active connections and requests drained successfully.")
		}

		stopServer()
	}()

	// 7. Start Listening
	slog.Info("Starting HTTP server",
		"addr", ":"+cfg.Port,
		"max_concurrent_workers", cfg.MaxConcurrent,
		"queue_timeout", cfg.QueueTimeout.String(),
		"endpoint", "GET / (50ms delay)",
	)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("HTTP server error", "error", err)
		os.Exit(1)
	}

	<-serverCtx.Done()
	slog.Info("Server stopped cleanly. Total rejected requests under load shedding",
		"rejected_count", limiter.rejected.Load(),
	)
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		return val
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if val, ok := os.LookupEnv(key); ok {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return defaultVal
}

func getEnvDuration(key string, defaultVal time.Duration) time.Duration {
	if val, ok := os.LookupEnv(key); ok {
		if ms, err := strconv.Atoi(val); err == nil {
			return time.Duration(ms) * time.Millisecond
		}
		if d, err := time.ParseDuration(val); err == nil {
			return d
		}
	}
	return defaultVal
}
