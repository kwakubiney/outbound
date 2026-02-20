package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"outbound/internal/agent"
	"outbound/internal/config"
	tunnelpb "outbound/proto"
)

func main() {
	var services config.ServiceFlag
	agentID := flag.String("id", "", "Agent identifier")
	edgeAddr := flag.String("edge", "localhost:8081", "Edge gRPC address")
	flag.Var(&services, "service", "Service mapping name=port (repeatable)")
	flag.Parse()

	if *agentID == "" {
		generated, err := generateID()
		if err != nil {
			log.Fatalf("failed to generate agent id: %v", err)
		}
		*agentID = generated
		log.Printf("generated agent id: %s", *agentID)
	}

	if len(services.Entries) == 0 {
		log.Fatalf("at least one --service name=port is required")
	}

	conn, err := grpc.NewClient(*edgeAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("failed to connect to edge: %v", err)
	}
	defer conn.Close()

	client := tunnelpb.NewTunnelServiceClient(conn)
	ctx := context.Background()
	stream, err := client.Connect(ctx)
	if err != nil {
		log.Fatalf("failed to open tunnel stream: %v", err)
	}

	agentClient := agent.NewClient(*agentID, agent.NormalizeServiceMap(services.Entries))
	log.Printf("connected to edge %s", *edgeAddr)
	if err := agentClient.Run(ctx, stream); err != nil {
		log.Fatalf("agent stopped: %v", err)
	}
}

func generateID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
