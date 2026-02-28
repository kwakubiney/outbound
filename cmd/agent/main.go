package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"flag"
	"log"
	mrand "math/rand"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"outbound/internal/agent"
	"outbound/internal/config"
	tunnelpb "outbound/proto"
)

func main() {
	var services config.ServiceFlag
	agentID := flag.String("id", "", "Agent identifier")
	edgeAddr := flag.String("edge", "localhost:8081", "Edge gRPC address")
	insecureMode := flag.Bool("insecure", false, "Disable TLS for the agent-to-edge gRPC connection (plain text)")
	flag.Var(&services, "service", "Service mapping name=port (repeatable)")
	flag.Parse()

	if *insecureMode {
		log.Printf("WARNING: the connection from this agent to the edge is unencrypted plain gRPC. Ensure the agent-to-edge link is secured at the network level (VPN, private network, or a TLS-terminating reverse proxy on both ports) before transmitting sensitive data.")
	}

	if *agentID == "" {
		generated, err := generateID()
		if err != nil {
			log.Fatalf("failed to generate agent id: %v", err)
		}
		*agentID = generated
		log.Printf("generated agent id: %s", *agentID)
	}

	if len(services.Entries) == 0 {
		log.Fatalf("at least one --service is required")
	}
	normalizedServices := agent.NormalizeServiceMap(services.Entries)

	conn, err := grpc.NewClient(*edgeAddr, grpc.WithTransportCredentials(buildCredentials(*insecureMode)))
	if err != nil {
		log.Fatalf("failed to connect to edge: %v", err)
	}
	defer conn.Close()

	client := tunnelpb.NewTunnelServiceClient(conn)
	ctx := context.Background()
	consecutiveFailures := 0
	for {
		stream, err := client.Connect(ctx)
		if err != nil {
			consecutiveFailures++
			delay := reconnectDelay(consecutiveFailures)
			log.Printf("failed to open tunnel stream: %v (retrying in %s)", err, delay)
			time.Sleep(delay)
			continue
		}

		agentClient := agent.NewClient(*agentID, normalizedServices)
		if consecutiveFailures == 0 {
			log.Printf("connected to edge %s", *edgeAddr)
		} else {
			log.Printf("reconnected to edge %s", *edgeAddr)
		}

		runErr := agentClient.Run(ctx, stream)

		consecutiveFailures = 0

		if runErr != nil {
			consecutiveFailures++
			delay := reconnectDelay(consecutiveFailures)
			log.Printf("tunnel stream dropped: %v (reconnecting in %s)", runErr, delay)
			time.Sleep(delay)
		}
	}
}

func generateID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func buildCredentials(insecureMode bool) credentials.TransportCredentials {
	if insecureMode {
		return insecure.NewCredentials()
	}
	return credentials.NewTLS(&tls.Config{})
}

func reconnectDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	const (
		baseDelay   = time.Second
		maxDelay    = 60 * time.Second
		jitterRatio = 0.2
	)

	delay := baseDelay
	for i := 1; i < attempt && delay < maxDelay; i++ {
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}

	jitterRange := int64(float64(delay) * jitterRatio)
	if jitterRange == 0 {
		return delay
	}
	jitter := time.Duration(mrand.Int63n((2 * jitterRange) + 1))
	return delay + jitter - time.Duration(jitterRange)
}
