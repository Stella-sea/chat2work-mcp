package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/deeix-ai/chat2work-mcp/internal/app"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML configuration")
	stdio := flag.Bool("stdio", false, "serve MCP over standard input/output")
	flag.Parse()
	config, err := app.LoadConfig(*configPath)
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	server, err := app.New(config)
	if err != nil {
		slog.Error("start server", "error", err)
		os.Exit(1)
	}
	defer server.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *stdio {
		if err := server.RunStdio(ctx); err != nil {
			slog.Error("stdio server stopped", "error", err)
			os.Exit(1)
		}
		return
	}

	httpServer := &http.Server{Addr: config.ListenAddr, Handler: server.HTTPHandler()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	slog.Info("chat2work-mcp listening", "addr", config.ListenAddr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("http server stopped", "error", err)
		os.Exit(1)
	}
}
