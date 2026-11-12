package gateway

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/yetone/magpie/internal/netproxy"
)

func encodeClaudeFixture(t *testing.T, encoding string, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.WriteCloser
	switch encoding {
	case "gzip":
		w = gzip.NewWriter(&buf)
	case "deflate":
		w = zlib.NewWriter(&buf)
	case "raw-deflate":
		var err error
		w, err = flate.NewWriter(&buf, flate.DefaultCompression)
		if err != nil {
			t.Fatal(err)
		}
	case "br":
		w = brotli.NewWriter(&buf)
	case "zstd":
		var err error
		w, err = zstd.NewWriter(&buf, zstd.WithEncoderConcurrency(1))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Exercise real sockets through the admitted subscription transport. The
// ordinary vendor transport must never inherit this compression behavior.
func TestClaudeHTTPCompressedWire(t *testing.T) {
	for _, encoding := range []string{"gzip", "deflate", "raw-deflate", "br", "zstd", "gzip, br"} {
		t.Run(encoding, func(t *testing.T) {
			plain := []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			parts := strings.Split(encoding, ", ")
			encoded := plain
			for _, part := range parts {
				encoded = encodeClaudeFixture(t, part, encoded)
			}
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept-Encoding") != claudeHTTPAcceptEncoding {
					t.Error("missing native compression advertisement")
				}
				w.Header().Set("Content-Encoding", strings.ReplaceAll(encoding, "raw-deflate", "deflate"))
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write(encoded)
			}))
			defer s.Close()
			roots := x509.NewCertPool()
			roots.AddCert(s.Certificate())
			pool := newClaudeHTTPPool(nil, roots)
			// Map the fixed API origin to the local fixture. Keep real TLS
			// verification against the fixture certificate's actual name.
			pool.DialTLSContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, s.Listener.Addr().String())
				if err != nil {
					return nil, err
				}
				return handshakeClaudeHTTP(ctx, conn, s.Certificate().DNSNames[0], roots)
			}
			wire := &claudeHTTPWire{pools: map[string]*http.Transport{"\x00": pool},
				next: claudeHTTPRoundTrip(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ordinary")), Header: http.Header{"Content-Encoding": {"br"}}}, nil
				})}
			defer wire.CloseIdleConnections()
			ctx, cancel := context.WithTimeout(netproxy.With(context.Background(), "direct"), 10*time.Second)
			defer cancel()
			ctx = context.WithValue(ctx, claudeHTTPKey{}, claudeHTTPCall{})
			req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", nil)
			res, err := wire.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			got, err := io.ReadAll(res.Body)
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatalf("compressed inference reply %q: %v", got, err)
			}
			if !res.Uncompressed || res.ContentLength != -1 || res.Header.Get("Content-Encoding") != "" || res.Header.Get("Content-Length") != "" {
				t.Fatal("decoded reply retained compressed metadata")
			}
			if req.Header.Get("Accept-Encoding") != "" {
				t.Fatal("transport changed caller's request")
			}
			ordinary, err := wire.RoundTrip(req.WithContext(context.Background()))
			if err != nil {
				t.Fatal(err)
			}
			defer ordinary.Body.Close()
			if ordinary.Uncompressed || ordinary.Header.Get("Content-Encoding") != "br" {
				t.Fatal("changed another vendor's compression")
			}
		})
	}
}

type claudeTrackedBody struct {
	io.Reader
	closed  bool
	start   sync.Once
	started chan struct{}
}

func (b *claudeTrackedBody) Close() error { b.closed = true; return nil }
func (b *claudeTrackedBody) Read(p []byte) (int, error) {
	if b.started != nil {
		b.start.Do(func() { close(b.started) })
	}
	return b.Reader.Read(p)
}

type claudeReadSignal struct {
	*claudeTrackedBody
	io.Closer
}

func (b *claudeReadSignal) Close() error { return b.Closer.Close() }

func TestClaudeHTTPCompressedFailures(t *testing.T) {
	for _, encoding := range []string{"gzip", "deflate", "br", "zstd", "unknown"} {
		t.Run(encoding, func(t *testing.T) {
			source := &claudeTrackedBody{Reader: strings.NewReader("\xffbroken")}
			res := &http.Response{Body: source, Header: http.Header{"Content-Encoding": {encoding}}}
			err := decodeClaudeHTTPResponse(res)
			if err == nil {
				_, err = io.ReadAll(res.Body)
				res.Body.Close()
			}
			if err == nil || !source.closed {
				t.Fatalf("malformed %s accepted or leaked: %v, closed=%v", encoding, err, source.closed)
			}
		})
	}
}

func TestClaudeHTTPCompressedStream(t *testing.T) {
	input, output := io.Pipe()
	res := &http.Response{Body: input, Header: http.Header{"Content-Encoding": {"gzip"}}}
	// No bytes are available yet: constructing the response must not read.
	if err := decodeClaudeHTTPResponse(res); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	release := make(chan struct{})
	go func() {
		w := gzip.NewWriter(output)
		_, err := io.WriteString(w, "first event\n")
		if err == nil {
			err = w.Flush()
		}
		<-release
		if err == nil {
			_, err = io.WriteString(w, "last event\n")
		}
		if closeErr := w.Close(); err == nil {
			err = closeErr
		}
		output.CloseWithError(err)
		done <- err
	}()
	t.Cleanup(func() { close(release); res.Body.Close(); <-done })
	got := make([]byte, len("first event\n"))
	if _, err := io.ReadFull(res.Body, got); err != nil || string(got) != "first event\n" {
		t.Fatalf("did not deliver event before stream finished: %q %v", got, err)
	}
}

func TestClaudeHTTPCompressedCloseUnblocks(t *testing.T) {
	for _, encoding := range []string{"gzip", "deflate", "br", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			input, output := io.Pipe()
			defer output.Close()
			started := make(chan struct{})
			source := &claudeReadSignal{&claudeTrackedBody{Reader: input, started: started}, input}
			res := &http.Response{Body: source, Header: http.Header{"Content-Encoding": {encoding}}}
			if err := decodeClaudeHTTPResponse(res); err != nil {
				t.Fatal(err)
			}
			read := make(chan error, 1)
			go func() {
				_, err := res.Body.Read(make([]byte, 1))
				read <- err
			}()
			<-started
			if err := res.Body.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-read:
				if err == nil {
					t.Fatal("closed compressed stream still readable")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("closing a compressed stream left a blocked reader")
			}
		})
	}
}
