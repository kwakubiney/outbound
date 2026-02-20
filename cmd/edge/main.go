package main

import (
	"flag"
	"log"
	"net"
	"net/http"

	"google.golang.org/grpc"
	"outbound/internal/edge"
	tunnelpb "outbound/proto"
)

func main() {
	httpAddr := flag.String("http-addr", ":8080", "HTTP listen address")
	grpcAddr := flag.String("grpc-addr", ":8081", "gRPC listen address")
	flag.Parse()

	server := edge.NewServer()

	grpcListener, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatalf("failed to listen on gRPC addr: %v", err)
	}

	grpcServer := grpc.NewServer()
	tunnelpb.RegisterTunnelServiceServer(grpcServer, server)

	go func() {
		log.Printf("gRPC listening on %s", *grpcAddr)
		if err := grpcServer.Serve(grpcListener); err != nil {
			log.Fatalf("gRPC server error: %v", err)
		}
	}()

	httpServer := &http.Server{Addr: *httpAddr, Handler: server}
	log.Printf("HTTP listening on %s", *httpAddr)
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatalf("HTTP server error: %v", err)
	}
}
