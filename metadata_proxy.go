package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/netutil"

	"github.com/GoogleCloudPlatform/k8s-metadata-proxy/metrics"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	concealedEndpoints = []string{
		"/0.1/meta-data/attributes/kube-env",
		"/computeMetadata/v1beta1/instance/attributes/kube-env",
		"/computeMetadata/v1/instance/attributes/kube-env",
	}
	concealedPatterns = []*regexp.Regexp{
		regexp.MustCompile("/0.1/meta-data/service-accounts/.+/identity"),
		regexp.MustCompile("/computeMetadata/v1beta1/instance/service-accounts/.+/identity"),
		regexp.MustCompile("/computeMetadata/v1/instance/service-accounts/.+/identity"),
	}
	knownPrefixes = []string{
		"/0.1/meta-data/",
		"/computeMetadata/v1beta1/",
		"/computeMetadata/v1/",
	}
	discoveryEndpoints = []string{
		"",
		"/",
		"/0.1",
		"/0.1/",
		"/0.1/meta-data",
		"/computeMetadata",
		"/computeMetadata/",
		"/computeMetadata/v1beta1",
		"/computeMetadata/v1",
	}
)

const servingGoroutines = 100

var (
	addr                = flag.String("addr", "127.0.0.1:988", "Address at which to listen and proxy")
	metricsAddr         = flag.String("metrics-addr", "127.0.0.1:989", "Address at which to publish metrics")
	filterResultBlocked = "filter_result_blocked"
	filterResultProxied = "filter_result_proxied"
)

func main() {
	flag.Parse()

	go func() {
		err := http.ListenAndServe(*metricsAddr, promhttp.Handler())
		log.Fatalf("Failed to start metrics: %v", err)
	}()
	log.Fatal(ListenAndServe(*addr, newMetadataHandler()))
}

func ListenAndServe(addr string, handler http.Handler) error {
	s := &http.Server{
		Addr:           addr,
		Handler:        handler,
		ReadTimeout:    60 * time.Second,
		WriteTimeout:   60 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}
	ln = tcpKeepAliveListener{ln.(*net.TCPListener)}
	ln = netutil.LimitListener(ln, servingGoroutines)
	return s.Serve(ln)
}

// xForwardedForStripper is identical to http.DefaultTransport except that it
// strips X-Forwarded-For headers.  It fulfills the http.RoundTripper
// interface.
type xForwardedForStripper struct{}

// RoundTrip wraps the http.DefaultTransport.RoundTrip method, and strips
// X-Forwarded-For headers, since httputil.ReverseProxy.ServeHTTP adds it but
// the GCE metadata server rejects requests with that header.
func (x xForwardedForStripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Del("X-Forwarded-For")
	return http.DefaultTransport.RoundTrip(req)
}

// responseWriter wraps the given http.ResponseWriter to record metrics.
type responseWriter struct {
	filterResult string
	http.ResponseWriter
}

func newResponseWriter(rw http.ResponseWriter) *responseWriter {
	return &responseWriter{
		"",
		rw,
	}
}

// WriteHeader records the header and writes the appropriate metric.
func (m responseWriter) WriteHeader(code int) {
	metrics.RequestCounter.WithLabelValues(m.filterResult, strconv.Itoa(code)).Inc()
	m.ResponseWriter.WriteHeader(code)
}

type metadataHandler struct {
	proxy *httputil.ReverseProxy
}

func newMetadataHandler() *metadataHandler {
	u, err := url.Parse("http://169.254.169.254")
	if err != nil {
		log.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.BufferPool = newBufferPool()

	proxy.Transport = xForwardedForStripper{}

	return &metadataHandler{
		proxy: proxy,
	}
}

// ServeHTTP serves http requests for the metadata proxy.
//
// Order of the checks below matters; specifically, concealment comes before
// proxies, since proxies just return immediately.
func (h *metadataHandler) ServeHTTP(hrw http.ResponseWriter, req *http.Request) {
	log.Println(req.URL.Path)

	// Wrap http.ResponseWriter to get collect metrics.
	rw := newResponseWriter(hrw)

	// Since we're stripping the X-Forwarded-For header that's added by
	// httputil.ReverseProxy.ServeHTTP, check for the header here and
	// refuse to serve if it's present.
	if _, ok := req.Header["X-Forwarded-For"]; ok {
		rw.filterResult = filterResultBlocked
		http.Error(rw, "Calls with X-Forwarded-For header are not allowed by the metadata proxy.", http.StatusForbidden)
	}
	// Check that the request isn't a recursive one.
	if req.URL.Query().Get("recursive") != "" {
		rw.filterResult = filterResultBlocked
		http.Error(rw, "?recursive calls are not allowed by the metadata proxy.", http.StatusForbidden)
		return
	}

	// Conceal kube-env and vm identity endpoints for known API versions.
	// Don't block unknown API versions, since we don't know if they have
	// the same paths.
	for _, e := range concealedEndpoints {
		if req.URL.Path == e {
			rw.filterResult = filterResultBlocked
			http.Error(rw, "This metadata endpoint is concealed.", http.StatusForbidden)
			return
		}
	}
	for _, p := range concealedPatterns {
		if p.MatchString(req.URL.Path) {
			rw.filterResult = filterResultBlocked
			http.Error(rw, "This metadata endpoint is concealed.", http.StatusForbidden)
			return
		}
	}

	// Allow proxy for known API versions, defined by prefixes and known
	// discovery endpoints.  Unknown API versions aren't allowed, since we
	// don't know what paths they have.
	for _, p := range knownPrefixes {
		if strings.HasPrefix(req.URL.Path, p) {
			rw.filterResult = filterResultProxied
			h.proxy.ServeHTTP(rw, req)
			return
		}
	}
	for _, e := range discoveryEndpoints {
		if req.URL.Path == e {
			rw.filterResult = filterResultProxied
			h.proxy.ServeHTTP(rw, req)
			return
		}
	}

	// If none of the above checks match, this is an unknown API, so block
	// it.
	rw.filterResult = filterResultBlocked
	http.Error(rw, "This metadata API is not allowed by the metadata proxy.", http.StatusForbidden)
}

type bufferPool chan []byte

func newBufferPool() bufferPool {
	bp := make(chan []byte, servingGoroutines)
	for i := 0; i < servingGoroutines; i++ {
		bp <- make([]byte, 32*1024)
	}
	return bp
}

func (bp bufferPool) Get() []byte {
	return <-bp
}

func (bp bufferPool) Put(b []byte) {
	bp <- b
}

// copied from net/http

// tcpKeepAliveListener sets TCP keep-alive timeouts on accepted
// connections. It's used by ListenAndServe and ListenAndServeTLS so
// dead TCP connections (e.g. closing laptop mid-download) eventually
// go away.
type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(3 * time.Minute)
	return tc, nil
}
