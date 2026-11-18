// transport_std.go — variant B's transport: the same forge served over net/http.
// Everything above the socket (routing, auth, domain) is identical to variant A;
// only the HTTP layer differs, which is exactly the experiment.

//go:build transport_std

package main

import (
	"io"
	"net"
	"net/http"
	"strings"
)

func serve(ln net.Listener, handler func(*httpRequest) httpResponse) error {
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
			if err != nil {
				w.WriteHeader(500)
				return
			}
			headers := map[string]string{}
			for name := range r.Header {
				key := strings.ToLower(name)
				if _, exists := headers[key]; !exists {
					headers[key] = r.Header.Get(name)
				}
			}
			path := r.URL.EscapedPath()
			if path == "" {
				path = "/"
			}
			resp := handler(&httpRequest{
				method:  r.Method,
				path:    path,
				query:   r.URL.Query(),
				headers: headers,
				body:    body,
				host:    r.Host,
			})
			for name, value := range resp.headers {
				w.Header().Set(name, value)
			}
			w.WriteHeader(resp.status)
			if len(resp.body) > 0 {
				w.Write(resp.body)
			}
		}),
	}
	return server.Serve(ln)
}
