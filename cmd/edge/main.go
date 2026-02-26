package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"outbound/internal/edge"
	tunnelpb "outbound/proto"
)

func main() {
	httpAddr := flag.String("http-addr", ":8080", "HTTP listen address")
	grpcAddr := flag.String("grpc-addr", ":8081", "gRPC listen address")
	requestTimeout := flag.Duration("request-timeout", 30*time.Second, "Timeout for proxied HTTP requests")
	maxRequestBody := flag.Int64("max-request-body", 10*1024*1024, "Maximum request body size in bytes (default 10MB)")
	shutdownTimeout := flag.Duration("shutdown-timeout", 30*time.Second, "Graceful shutdown timeout")
	flag.Parse()

	server := edge.NewServer(edge.ServerConfig{
		RequestTimeout: *requestTimeout,
		MaxRequestBody: *maxRequestBody,
	})

	grpcListener, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatalf("failed to listen on gRPC addr: %v", err)
	}

	grpcServer := grpc.NewServer()
	tunnelpb.RegisterTunnelServiceServer(grpcServer, server)

	// Start gRPC server in background
	go func() {
		log.Printf("gRPC listening on %s", *grpcAddr)
		if err := grpcServer.Serve(grpcListener); err != nil {
			log.Printf("gRPC server stopped: %v", err)
		}
	}()

	httpServer := &http.Server{
		Addr:              *httpAddr,
		Handler:           server,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Start HTTP server in background
	go func() {
		log.Printf("HTTP listening on %s", *httpAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server stopped: %v", err)
		}
	}()

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("received signal %v, initiating graceful shutdown...", sig)

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancel()

	// Stop accepting new gRPC connections, finish existing ones
	grpcServer.GracefulStop()
	log.Printf("gRPC server stopped")

	// Stop accepting new HTTP connections, finish existing ones
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	} else {
		log.Printf("HTTP server stopped")
	}

	log.Printf("shutdown complete")
}
