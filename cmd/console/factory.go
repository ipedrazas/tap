package main

import (
	_ "embed"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

//go:embed factory.html
var factoryHTML string

// factoryProxy forwards /api/factory/* to the factory's /v1/* API in
// tap-factory, whose NetworkPolicy admits only this console. The caller's
// identity goes along as x-tap-user, taken from the ID token the gateway
// verified; the token itself is not forwarded.
func factoryProxy(target *url.URL) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = strings.TrimSuffix(target.Path, "/") + "/v1/" + strings.TrimPrefix(pr.In.URL.Path, "/api/factory/")
			pr.Out.URL.RawPath = ""
			pr.Out.Header.Del("x-tap-identity")
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Set("x-tap-user", user(pr.In))
		},
		// Job events are a server-sent event stream.
		FlushInterval: -1,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user(r) == "" {
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
		// Only JSON bodies change state: a cross-site form cannot send one
		// without a CORS preflight, which this server never allows.
		if r.Method != http.MethodGet {
			ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if ct != "application/json" {
				http.Error(w, "want application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		rp.ServeHTTP(w, r)
	})
}

func factoryPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; form-action 'none'")
	_, _ = w.Write([]byte(factoryHTML))
}
