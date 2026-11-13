package gui

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// delayedServiceEOF makes the proxy's final body read happen after it has
// flushed response headers. A real transport may schedule its writer this way.
type delayedServiceEOF struct {
	io.ReadCloser
	flushed <-chan struct{}
	ctx     context.Context
	read    bool
}

func (b *delayedServiceEOF) Read(p []byte) (int, error) {
	if b.read {
		select {
		case <-b.flushed:
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		}
	}
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.read = true
		return n, nil
	}
	return n, err
}

type serviceFlushNotice struct {
	http.ResponseWriter
	flushed chan struct{}
	once    sync.Once
}

func (w *serviceFlushNotice) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *serviceFlushNotice) Flush() {
	w.ResponseWriter.(http.Flusher).Flush()
	w.once.Do(func() { close(w.flushed) })
}

func TestPluginPageServiceEarlyResponse(t *testing.T) {
	f := newFakeService(t)
	servicePlugins(t, `{"port": `+strconv.Itoa(f.port)+`}`, nil)
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:3425")
	page := serviceMux(true)
	p := listed(t, page)
	h := webHandler("3430", onePortWebKey, 0, nil, page)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flushed := make(chan struct{})
		r.Body = &delayedServiceEOF{ReadCloser: r.Body, flushed: flushed, ctx: r.Context()}
		h.ServeHTTP(&serviceFlushNotice{ResponseWriter: w, flushed: flushed}, r)
	}))
	t.Cleanup(srv.Close)
	base := srv.URL + strings.TrimSuffix(p.URL, "index.html") + pageServicePath
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"api/chat", strings.NewReader(`{}`))
	req.Header.Set("Origin", "null")
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	if !sc.Scan() || sc.Text() != "data: first" {
		t.Fatalf("first event: %q, %v", sc.Text(), sc.Err())
	}
	close(f.more)
	var rest []string
	for sc.Scan() {
		rest = append(rest, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Errorf("stream interrupted: %v", err)
	}
	if !strings.Contains(strings.Join(rest, "\n"), "data: second") {
		t.Errorf("second event missing: %q", rest)
	}
}
