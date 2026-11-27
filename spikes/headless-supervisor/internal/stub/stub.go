// Package stub is a local HTTP server that answers every request with a
// scripted provider error, so the error cases never reach a real API.
package stub

import (
	"io"
	"net"
	"net/http"
	"sync"
)

// Response is one scripted reply.
type Response struct {
	Status int
	Header map[string]string
	Body   string
}

// DefaultScript is a rate limit, an overload, then a credit-style rejection.
// The last response repeats once the script is exhausted.
func DefaultScript() []Response {
	return []Response{
		{
			Status: http.StatusTooManyRequests,
			Header: map[string]string{
				"retry-after":                        "1",
				"anthropic-ratelimit-unified-status": "rejected",
			},
			Body: `{"type":"error","error":{"type":"rate_limit_error","message":"stub: rate limited"}}`,
		},
		{
			Status: 529,
			Body:   `{"type":"error","error":{"type":"overloaded_error","message":"stub: overloaded"}}`,
		},
		{
			Status: http.StatusBadRequest,
			Body:   `{"type":"error","error":{"type":"invalid_request_error","message":"stub: your credit balance is too low to access the API"}}`,
		},
	}
}

// Server is a running stub.
type Server struct {
	URL string

	srv    *http.Server
	script []Response
	mu     sync.Mutex
	hits   int
}

// Start listens on 127.0.0.1 on a free port and serves script in order.
func Start(script []Response) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{URL: "http://" + ln.Addr().String(), script: script}
	s.srv = &http.Server{Handler: http.HandlerFunc(s.handle)}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// handle drains the body and never inspects headers or contents: they carry
// the caller's credentials.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	s.mu.Lock()
	i := min(s.hits, len(s.script)-1)
	s.hits++
	s.mu.Unlock()
	if i < 0 {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	resp := s.script[i]
	for k, v := range resp.Header {
		w.Header().Set(k, v)
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(resp.Status)
	_, _ = io.WriteString(w, resp.Body)
}

// Hits is the number of requests answered so far.
func (s *Server) Hits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

func (s *Server) Close() error { return s.srv.Close() }
