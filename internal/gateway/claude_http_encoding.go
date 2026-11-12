package gateway

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// Observed on native Claude Code's Bun fetch requests. Keep decoding here,
// scoped to the subscription transport, rather than changing other vendors.
const claudeHTTPAcceptEncoding = "gzip, deflate, br, zstd"

func decodeClaudeHTTPResponse(res *http.Response) error {
	if res.Body == nil || res.Body == http.NoBody || (res.Request != nil && res.Request.Method == http.MethodHead) {
		return nil
	}
	value := strings.Join(res.Header.Values("Content-Encoding"), ",")
	if value == "" {
		return nil
	}
	encodings := strings.Split(strings.ToLower(value), ",")
	for i, encoding := range encodings {
		encodings[i] = strings.TrimSpace(encoding)
		switch encodings[i] {
		case "gzip", "deflate", "br", "zstd", "identity":
		default:
			res.Body.Close()
			return fmt.Errorf("Claude HTTP: unsupported content encoding %q", encoding)
		}
	}
	res.Body = &claudeHTTPDecodedBody{source: res.Body, encodings: encodings}
	res.ContentLength = -1
	res.Uncompressed = true
	res.Header.Del("Content-Encoding")
	res.Header.Del("Content-Length")
	return nil
}

// Decoder creation can read bytes from a stream. Defer it to Read so receiving
// headers never waits for the first compressed SSE event.
type claudeHTTPDecodedBody struct {
	mu        sync.Mutex
	closeOnce sync.Once
	closeErr  error
	closed    bool
	source    io.ReadCloser
	encodings []string
	reader    io.Reader
	closers   []func() error
	err       error
}

func (b *claudeHTTPDecodedBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, http.ErrBodyReadAfterClose
	}
	if b.err != nil {
		return 0, b.err
	}
	if b.reader == nil {
		b.reader = b.source
		for i := len(b.encodings) - 1; i >= 0; i-- {
			var next io.Reader
			var closeDecoder func() error
			switch b.encodings[i] {
			case "identity":
				continue
			case "gzip":
				var decoder *gzip.Reader
				decoder, b.err = gzip.NewReader(b.reader)
				if b.err == nil {
					next, closeDecoder = decoder, decoder.Close
				}
			case "deflate":
				// HTTP deflate is zlib-wrapped; accept the raw form too,
				// as native fetch does for older servers.
				buffer := bufio.NewReader(b.reader)
				var head []byte
				head, b.err = buffer.Peek(2)
				if b.err == nil {
					var decoder io.ReadCloser
					if head[0]&15 == 8 && (int(head[0])<<8|int(head[1]))%31 == 0 {
						decoder, b.err = zlib.NewReader(buffer)
					} else {
						decoder = flate.NewReader(buffer)
					}
					if b.err == nil {
						next, closeDecoder = decoder, decoder.Close
					}
				}
			case "br":
				next = brotli.NewReader(b.reader)
			case "zstd":
				var decoder *zstd.Decoder
				decoder, b.err = zstd.NewReader(b.reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
				if b.err == nil {
					next = decoder
					closeDecoder = func() error { decoder.Close(); return nil }
				}
			}
			if b.err != nil {
				b.err = fmt.Errorf("Claude HTTP %s response: %w", b.encodings[i], b.err)
				return 0, b.err
			}
			b.reader = next
			if closeDecoder != nil {
				b.closers = append(b.closers, closeDecoder)
			}
		}
	}
	return b.reader.Read(p)
}

func (b *claudeHTTPDecodedBody) Close() error {
	b.closeOnce.Do(func() {
		// Interrupt a blocked read before waiting for its decoder state.
		b.closeErr = b.source.Close()
		b.mu.Lock()
		defer b.mu.Unlock()
		b.closed = true
		for i := len(b.closers) - 1; i >= 0; i-- {
			if err := b.closers[i](); b.closeErr == nil {
				b.closeErr = err
			}
		}
	})
	return b.closeErr
}
