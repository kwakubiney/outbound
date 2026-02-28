package tunneltest

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBasicRequest(t *testing.T) {
	h := NewHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "ok")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("echo:" + r.Method + ":" + r.URL.Path))
	}, HarnessOptions{})

	resp, body := doRequest(t, h.HTTPClient(), http.MethodGet, h.URL("/hello"), nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status %d, body=%q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Upstream"); got != "ok" {
		t.Fatalf("expected X-Upstream header, got %q", got)
	}
	if body != "echo:GET:/hello" {
		t.Fatalf("unexpected body %q", body)
	}
}

func TestAuthAccepted(t *testing.T) {
	h := NewHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}, HarnessOptions{AuthSecret: "secret-1", AgentToken: "secret-1"})

	resp, body := doRequest(t, h.HTTPClient(), http.MethodGet, h.URL("/auth-ok"), nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", resp.StatusCode, body)
	}
	if body != "ok" {
		t.Fatalf("unexpected body %q", body)
	}
}

func TestAuthRejected(t *testing.T) {
	h := NewHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("should-not-reach"))
	}, HarnessOptions{AuthSecret: "secret-1", AgentToken: "wrong-secret", SkipInitialReady: true})

	// Confirm the agent never registers: probe until deadline and expect 503 every time.
	deadline := time.Now().Add(500 * time.Millisecond)
	client := h.HTTPClient()
	for time.Now().Before(deadline) {
		resp, body := doRequest(t, client, http.MethodGet, h.URL("/auth-fail"), nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 while agent auth is rejected, got %d body=%q", resp.StatusCode, body)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestConcurrentRequests(t *testing.T) {
	h := NewHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Query().Get("id")))
	}, HarnessOptions{})

	client := h.HTTPClient()
	const concurrent = 50

	errCh := make(chan error, concurrent)
	var wg sync.WaitGroup
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := fmt.Sprintf("%d", i)
			resp, body := doRequest(t, client, http.MethodGet, h.URL("/echo?id="+want), nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errCh <- fmt.Errorf("id=%s status=%d body=%q", want, resp.StatusCode, body)
				return
			}
			if body != want {
				errCh <- fmt.Errorf("id=%s got body %q", want, body)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}
}

func TestAgentDisconnectMidRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	h := NewHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/disconnect" {
			w.WriteHeader(http.StatusOK)
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("late"))
	}, HarnessOptions{RequestTimeout: 350 * time.Millisecond})

	client := h.HTTPClient()
	req, err := http.NewRequest(http.MethodGet, h.URL("/disconnect"), nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resultCh := make(chan struct {
		status int
		body   string
		err    error
	}, 1)

	go func() {
		resp, err := client.Do(req)
		if err != nil {
			resultCh <- struct {
				status int
				body   string
				err    error
			}{err: err}
			return
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		resultCh <- struct {
			status int
			body   string
			err    error
		}{status: resp.StatusCode, body: string(data)}
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request never reached upstream")
	}

	h.KillAgent()
	close(release)

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("request failed with transport error: %v", result.err)
		}
		if result.status != http.StatusBadGateway && result.status != http.StatusGatewayTimeout {
			t.Fatalf("expected status 502/504, got %d body=%q", result.status, result.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request did not complete after agent disconnect")
	}
}

func TestAgentReconnect(t *testing.T) {
	h := NewHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}, HarnessOptions{})

	h.KillAgent()
	if err := h.WaitAgentReconnected(3 * time.Second); err != nil {
		t.Fatalf("agent did not reconnect: %v", err)
	}

	resp, body := doRequest(t, h.HTTPClient(), http.MethodGet, h.URL("/after-reconnect"), nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status %d body=%q", resp.StatusCode, body)
	}
	if body != "ok" {
		t.Fatalf("unexpected body %q", body)
	}
}

func TestUpstreamSlowResponseTimesOut(t *testing.T) {
	h := NewHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("too-late"))
	}, HarnessOptions{RequestTimeout: 120 * time.Millisecond})

	resp, body := doRequest(t, h.HTTPClient(), http.MethodGet, h.URL("/slow"), nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d body=%q", resp.StatusCode, body)
	}
	if !strings.Contains(body, "timeout") {
		t.Fatalf("expected timeout message, got %q", body)
	}
}

func doRequest(t *testing.T, client *http.Client, method, rawURL string, body io.Reader) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	bytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("failed to close response body: %v", err)
	}

	resp.Body = io.NopCloser(strings.NewReader(string(bytes)))
	return resp, string(bytes)
}
