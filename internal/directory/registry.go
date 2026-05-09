// Package directory is a local discovery helper (not part of A2A spec).
// Agents self-register their Agent Card URL; clients list peers.
package directory

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

type Entry struct {
	URL      string    `json:"url"`
	LastSeen time.Time `json:"lastSeen"`
}

type Registry struct {
	mu      sync.RWMutex
	entries map[string]Entry // url -> entry
	log     *slog.Logger
	ttl     time.Duration
}

func New(log *slog.Logger) *Registry {
	r := &Registry{entries: map[string]Entry{}, log: log, ttl: 90 * time.Second}
	go r.gc()
	return r
}

func (r *Registry) gc() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-r.ttl)
		r.mu.Lock()
		for k, e := range r.entries {
			if e.LastSeen.Before(cutoff) {
				delete(r.entries, k)
			}
		}
		r.mu.Unlock()
	}
}

func (r *Registry) Register(url string) {
	r.mu.Lock()
	r.entries[url] = Entry{URL: url, LastSeen: time.Now()}
	r.mu.Unlock()
}

func (r *Registry) Unregister(url string) {
	r.mu.Lock()
	delete(r.entries, url)
	r.mu.Unlock()
}

func (r *Registry) List() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Entry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	return out
}

func (r *Registry) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.List())
	})
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.URL == "" {
			http.Error(w, "bad request", 400)
			return
		}
		r.Register(body.URL)
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /unregister", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.URL == "" {
			http.Error(w, "bad request", 400)
			return
		}
		r.Unregister(body.URL)
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /heartbeat", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.URL == "" {
			http.Error(w, "bad request", 400)
			return
		}
		r.Register(body.URL) // refreshes LastSeen
		w.WriteHeader(204)
	})
	return mux
}
