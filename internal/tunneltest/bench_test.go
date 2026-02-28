package tunneltest

import (
	"io"
	"net/http"
	"testing"
)

func BenchmarkThroughput(b *testing.B) {
	h := NewHarness(b, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}, HarnessOptions{})

	client := h.HTTPClient()
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		req, err := http.NewRequest(http.MethodGet, h.URL("/bench"), nil)
		if err != nil {
			b.Fatalf("failed to build request: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			b.Fatalf("request failed: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("unexpected status %d", resp.StatusCode)
		}
	}
}

func BenchmarkConcurrent(b *testing.B) {
	h := NewHarness(b, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}, HarnessOptions{})

	client := h.HTTPClient()
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, err := http.NewRequest(http.MethodGet, h.URL("/bench-parallel"), nil)
			if err != nil {
				b.Fatalf("failed to build request: %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				b.Fatalf("request failed: %v", err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				b.Fatalf("unexpected status %d", resp.StatusCode)
			}
		}
	})
}
