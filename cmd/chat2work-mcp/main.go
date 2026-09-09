package main

import (
	"context"
	"flag"
	"log"
	"net/http"

	"github.com/deeix-ai/chat2work-mcp/internal/app"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML configuration")
	stdio := flag.Bool("stdio", false, "serve MCP over standard input/output")
	flag.Parse()
	config, err := app.LoadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	server, err := app.New(config)
	if err != nil {
		log.Fatal(err)
	}
	if *stdio {
		if err := server.RunStdio(context.Background()); err != nil {
			log.Fatal(err)
		}
		return
	}
	log.Printf("chat2work-mcp listening on %s", config.ListenAddr)
	if err := http.ListenAndServe(config.ListenAddr, server.HTTPHandler()); err != nil {
		log.Fatal(err)
	}
}
