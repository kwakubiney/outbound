package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/soheilhy/cmux"
	"google.golang.org/grpc"
	"outbound/internal/edge"
	tunnelpb "outbound/proto"
)

func main() {
	addr := flag.String("addr", ":8080", "Listen address for both HTTP and gRPC")
	requestTimeout := flag.Duration("request-timeout", 30*time.Second, "Timeout for proxied HTTP requests")
	keepaliveInterval := flag.Duration("keepalive-interval", 15*time.Second, "Interval between edge keepalive pings to agents")
	keepaliveTimeout := flag.Duration("keepalive-timeout", 5*time.Second, "Time to wait for agent pong before dropping session")
	authSecret := flag.String("auth-secret", "", "Shared auth secret required for agent registration (empty disables auth)")
	shutdownTimeout := flag.Duration("shutdown-timeout", 30*time.Second, "Graceful shutdown timeout")
	flag.Parse()

	if strings.TrimSpace(*authSecret) == "" {
		*authSecret = envOr("OUTBOUND_AUTH_SECRET", "")
	}
	if *authSecret == "" {
		log.Printf("WARNING: no --auth-secret configured; all agent connections accepted")
	}

	server := edge.NewServer(edge.ServerConfig{
		RequestTimeout:    *requestTimeout,
		KeepaliveInterval: *keepaliveInterval,
		KeepaliveTimeout:  *keepaliveTimeout,
		AuthSecret:        *authSecret,
	})

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", *addr, err)
	}

	mux := cmux.New(listener)
	grpcListener := mux.MatchWithWriters(cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"))
	httpListener := mux.Match(cmux.Any())

	grpcServer := grpc.NewServer()
	tunnelpb.RegisterTunnelServiceServer(grpcServer, server)

	httpServer := &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("gRPC listening on %s", *addr)
		if err := grpcServer.Serve(grpcListener); err != nil {
			log.Printf("gRPC server stopped: %v", err)
		}
	}()

	go func() {
		log.Printf("HTTP listening on %s", *addr)
		if err := httpServer.Serve(httpListener); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server stopped: %v", err)
		}
	}()

	go func() {
		if err := mux.Serve(); err != nil {
			log.Printf("mux stopped: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("received signal %v, initiating graceful shutdown...", sig)

	ctx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancel()

	grpcServer.GracefulStop()
	log.Printf("gRPC server stopped")

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	} else {
		log.Printf("HTTP server stopped")
	}

	log.Printf("shutdown complete")
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
