package harness

import (
	"bytes"
	"io"
	"net/http"
	"sync"
)

// cachingProxy is a deliberately minimal exact-match cache in front of one
// upstream. It exists only so the cache scenario can be proven non-vacuous:
// a check that never returns "pass" is indistinguishable from a broken one.
type cachingProxy struct {
	upstream string
	mu       sync.Mutex
	entries  map[string][]byte
}

func newCachingProxy(upstream string) http.Handler {
	return &cachingProxy{upstream: upstream, entries: map[string][]byte{}}
}

func (p *cachingProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	key := string(body)

	p.mu.Lock()
	cached, hit := p.entries[key]
	p.mu.Unlock()

	if hit {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "HIT")
		_, _ = w.Write(cached)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.upstream, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == 200 {
		p.mu.Lock()
		p.entries[key] = out
		p.mu.Unlock()
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}
