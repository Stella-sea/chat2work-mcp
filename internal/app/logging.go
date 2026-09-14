package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

type contextKey int

const requestIDKey contextKey = iota

// newLogger builds a JSON logger on stderr. Stdout is reserved for the stdio
// MCP transport, so logs must never go there.
func newLogger(level string) *slog.Logger {
	var parsed slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		parsed = slog.LevelDebug
	case "warn", "warning":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	default:
		parsed = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: parsed}))
}

// log returns the app logger, falling back to the default logger so handlers
// constructed directly in tests never panic.
func (a *App) log() *slog.Logger {
	if a != nil && a.logger != nil {
		return a.logger
	}
	return slog.Default()
}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

func requestIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func newRequestID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(buf[:])
}

// statusRecorder captures the response status for logging while preserving the
// optional interfaces the MCP streamable handler needs for SSE.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if !r.wrote {
		r.status = http.StatusOK
		r.wrote = true
	}
	return r.ResponseWriter.Write(data)
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// logRequests assigns or propagates a request ID, then logs one structured line
// per HTTP request. Health probes are logged at debug to keep logs quiet.
func (a *App) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if id == "" {
			id = newRequestID()
		}
		ctx := withRequestID(r.Context(), id)
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(recorder, r.WithContext(ctx))
		level := slog.LevelInfo
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			level = slog.LevelDebug
		}
		a.log().LogAttrs(ctx, level, "http_request",
			slog.String("request_id", id),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", recorder.status),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		)
	})
}
