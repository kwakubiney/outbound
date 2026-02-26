package main

import (
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"os"
	"time"
)

var indexTmpl = template.Must(template.New("index").Parse(`<!DOCTYPE html>
<html>
<head><title>Outbound Demo</title>
<style>
  body { font-family: monospace; max-width: 600px; margin: 60px auto; padding: 0 20px; background: #0d0d0d; color: #e0e0e0; }
  h1 { color: #7effa0; }
  .kv { display: flex; gap: 16px; margin: 8px 0; }
  .k { color: #888; width: 160px; flex-shrink: 0; }
  .v { color: #fff; }
  hr { border-color: #333; margin: 24px 0; }
</style>
</head>
<body>
<h1>outbound tunnel demo</h1>
<hr>
<div class="kv"><span class="k">time</span><span class="v">{{.Time}}</span></div>
<div class="kv"><span class="k">host</span><span class="v">{{.Host}}</span></div>
<div class="kv"><span class="k">remote addr</span><span class="v">{{.RemoteAddr}}</span></div>
<div class="kv"><span class="k">method</span><span class="v">{{.Method}}</span></div>
<div class="kv"><span class="k">path</span><span class="v">{{.Path}}</span></div>
<div class="kv"><span class="k">user-agent</span><span class="v">{{.UserAgent}}</span></div>
<hr>
<p style="color:#555">this response came from a local Go server tunnelled through <strong style="color:#7effa0">edge.egyir.xyz</strong></p>
</body>
</html>`))

func main() {
	port := "3000"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data := struct {
			Time       string
			Host       string
			RemoteAddr string
			Method     string
			Path       string
			UserAgent  string
		}{
			Time:       time.Now().UTC().Format(time.RFC3339),
			Host:       r.Host,
			RemoteAddr: r.RemoteAddr,
			Method:     r.Method,
			Path:       r.URL.Path,
			UserAgent:  r.UserAgent(),
		}
		if err := indexTmpl.Execute(w, data); err != nil {
			log.Printf("template error: %v", err)
		}
	})

	http.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"time": time.Now().UTC(),
		})
	})

	log.Printf("demo server listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
