// Command go-llm-mcp runs the go-llm MCP server as a standalone binary.
// It supports stdio and HTTP/2 (h2c or TLS) transports.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kstruzzieri/go-llm/mcp"
)

func main() {
	transport := flag.String("transport", "stdio", "Transport mode: stdio or http")
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
	ollamaURL := flag.String("ollama-url", "", "Ollama server URL (default: from config or http://localhost:11434)")
	configPath := flag.String("config", "", "Path to models.json (default: auto-discover)")
	ragDB := flag.String("rag-db", "", "RAG database path (default: ~/.local/share/go-llm/rag.db)")
	noRAG := flag.Bool("no-rag", false, "Disable RAG tools")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file (enables HTTPS)")
	tlsKey := flag.String("tls-key", "", "TLS private key file")
	flag.Parse()

	var opts []mcp.Option
	if *ollamaURL != "" {
		opts = append(opts, mcp.WithOllamaURL(*ollamaURL))
	}
	if *configPath != "" {
		opts = append(opts, mcp.WithConfig(*configPath))
	}
	if *ragDB != "" {
		opts = append(opts, mcp.WithRAGPath(*ragDB))
	}
	if *noRAG {
		opts = append(opts, mcp.WithRAGDisabled())
	}
	if *tlsCert != "" && *tlsKey != "" {
		opts = append(opts, mcp.WithTLS(*tlsCert, *tlsKey))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := mcp.NewServer(ctx, opts...)
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	// Handle shutdown signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "received %s, shutting down...\n", sig)
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "shutdown error: %v\n", err)
		}
		cancel()
	}()

	switch *transport {
	case "stdio":
		if err := srv.ListenStdio(ctx); err != nil {
			log.Fatalf("stdio server error: %v", err)
		}
	case "http":
		fmt.Fprintf(os.Stderr, "go-llm MCP server listening on %s\n", *addr)
		if err := srv.ListenHTTP(ctx, *addr); err != nil {
			log.Fatalf("http server error: %v", err)
		}
	default:
		log.Fatalf("unknown transport: %q (use stdio or http)", *transport)
	}
}
