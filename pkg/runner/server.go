package runner

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

const maxRequestBody = 1 << 20

// Handler serves POST /v1/call and GET /healthz. Calls need the shared
// bearer token; at most `concurrency` tools run at once.
func Handler(r *Runner, token string, concurrency int) http.Handler {
	sem := make(chan struct{}, concurrency)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/call", func(w http.ResponseWriter, req *http.Request) {
		got, _ := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
		if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var call Request
		dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxRequestBody))
		dec.UseNumber()
		if err := dec.Decode(&call); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-req.Context().Done():
			return
		}
		resp := r.Call(req.Context(), call)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	return mux
}
