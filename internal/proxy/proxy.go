// Package proxy is the capture proxy between an agent and its model provider
// (SPEC §7.2). It forwards bytes untouched both ways, passes every header
// through (and never logs one), and hands copies of the request and the full
// response to a parser after forwarding; parsing never affects the agent.
package proxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/andybalholm/brotli"
)

var nextID atomic.Uint64

// maxTee bounds what is copied for parsing; larger exchanges are forwarded
// but not recorded.
const maxTee = 64 << 20

// Exchange is one request and its response, copied for the parser.
type Exchange struct {
	ID          uint64 // pairs the request and response halves
	Path        string
	Header      http.Header // request headers; never logged
	Request     []byte
	Status      int
	ContentType string
	Response    []byte // decoded when the response was gzip or deflate
	Truncated   bool
}

// Observer receives each exchange twice, in order: once when the request
// arrives (Response nil), so request-side work such as sampling the files a
// finished tool wrote happens before the agent can act on the reply, and once
// when the response has been read in full.
type Observer interface {
	Request(Exchange)
	Response(Exchange)
}

// Handler forwards requests under prefix to upstream and hands copies to obs
// through one FIFO queue, so the observer sees exchanges in arrival order
// without slowing the agent.
func Handler(prefix string, upstream *url.URL, obs Observer) http.Handler {
	q := make(chan func(), 1024)
	go func() {
		for job := range q {
			job()
		}
	}()
	enqueue := func(job func()) {
		select {
		case q <- job:
		default: // the recorder is far behind: drop rather than block the agent
		}
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.URL.Path = joinPath(upstream.Path, strings.TrimPrefix(pr.In.URL.Path, prefix))
			pr.Out.URL.RawPath = ""
			pr.Out.Host = upstream.Host
		},
		FlushInterval: -1,
		Transport:     &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true},
		ErrorLog:      nil,
		ModifyResponse: func(resp *http.Response) error {
			ex := resp.Request.Context().Value(exchangeKey{}).(*Exchange)
			ex.Status = resp.StatusCode
			ex.ContentType = resp.Header.Get("Content-Type")
			enc := resp.Header.Get("Content-Encoding")
			resp.Body = &teeBody{rc: resp.Body, done: func(b []byte, truncated bool) {
				ex.Response, ex.Truncated = decode(enc, b), truncated || ex.Truncated
				done := *ex
				enqueue(func() { obs.Response(done) })
			}}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxTee+1))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ex := &Exchange{ID: nextID.Add(1), Path: strings.TrimPrefix(r.URL.Path, prefix), Header: r.Header.Clone(), Truncated: len(body) > maxTee}
		if !ex.Truncated {
			ex.Request = body
		}
		arrived := *ex
		enqueue(func() { obs.Request(arrived) })
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		rp.ServeHTTP(w, r.WithContext(withExchange(r.Context(), ex)))
	})
}

func joinPath(base, p string) string {
	if base == "" || base == "/" {
		return p
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(p, "/")
}

// teeBody copies what the agent reads, and reports it once at EOF or close.
type teeBody struct {
	rc        io.ReadCloser
	buf       bytes.Buffer
	truncated bool
	reported  bool
	done      func([]byte, bool)
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 && !t.truncated {
		if t.buf.Len()+n > maxTee {
			t.truncated = true
			t.buf.Reset()
		} else {
			t.buf.Write(p[:n])
		}
	}
	if err == io.EOF {
		t.report()
	}
	return n, err
}

func (t *teeBody) Close() error {
	t.report()
	return t.rc.Close()
}

func (t *teeBody) report() {
	if !t.reported {
		t.reported = true
		t.done(t.buf.Bytes(), t.truncated)
	}
}

// decode undoes gzip or deflate on the parser's copy only.
func decode(enc string, b []byte) []byte {
	var r io.Reader
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "", "identity":
		return b
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil
		}
		r = zr
	case "deflate":
		r = flate.NewReader(bytes.NewReader(b))
	case "br":
		r = brotli.NewReader(bytes.NewReader(b))
	default:
		return nil // forwarded, not parsed
	}
	out, err := io.ReadAll(io.LimitReader(r, maxTee))
	if err != nil {
		return nil
	}
	return out
}
