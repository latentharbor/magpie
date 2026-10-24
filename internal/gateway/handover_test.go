package gateway

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// acceptOnClose holds an already accepted connection until the listener
// closes: exactly the Accept/Shutdown overlap a handover must preserve.
type acceptOnClose struct {
	net.Listener
	accepted chan struct{}
	closed   chan struct{}
	once     sync.Once
}

func (l *acceptOnClose) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		close(l.accepted)
		<-l.closed
	}
	return c, err
}

func (l *acceptOnClose) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

func TestHandoverKeepsAConnectionAcceptedAsItStops(t *testing.T) {
	fresh(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"accepted"},"finish_reason":"stop"}]}`)
	}))
	defer up.Close()
	if err := provider.Save(provider.Provider{ID: "plan", Name: "Plan", Key: "upstream", Chat: up.URL + "/v1", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &acceptOnClose{Listener: ln, accepted: make(chan struct{}), closed: make(chan struct{})}
	defer l.Close()
	t.Setenv("MAGPIE_ADDR", ln.Addr().String())
	Inherit(l, nil)
	s := New()
	stop, done := listening(t, s)
	defer func() {
		stop()
		l.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	answer := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 10 * time.Second}
		r, err := client.Post("http://"+ln.Addr().String()+"/v1/chat/completions", "application/json", strings.NewReader(chatReq))
		if err == nil {
			defer r.Body.Close()
			b, readErr := io.ReadAll(r.Body)
			err = readErr
			if err == nil && (r.StatusCode != 200 || !strings.Contains(string(b), "accepted")) {
				err = fmt.Errorf("accepted request: %d %s", r.StatusCode, b)
			}
		}
		answer <- err
	}()
	select {
	case <-l.accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never reached Accept")
	}
	s.HandOff()
	stop()
	if err := <-answer; err != nil {
		t.Fatalf("the connection accepted during handover was lost: %v", err)
	}
}

func TestHandoverServesTheNextRequestOnAnOldConnection(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "127.0.0.1:0")
	s := New()
	stop, done := listening(t, s)
	stopped := false
	defer func() {
		stop()
		if !stopped {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("the old server did not drain")
			}
		}
	}()
	c, err := net.DialTimeout("tcp", s.Addr(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(c)
	ask := func() *http.Response {
		t.Helper()
		if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		r, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("the old connection lost its next request: %v", err)
		}
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		return r
	}
	if r := ask(); r.StatusCode != 200 || r.Close {
		t.Fatalf("the initial connection is not reusable: %d close=%v", r.StatusCode, r.Close)
	}
	s.HandOff()
	stop()
	select {
	case <-done:
		stopped = true
		t.Fatal("the old server closed a reusable connection during handover")
	default:
	}
	if r := ask(); r.StatusCode != 200 || !r.Close {
		t.Fatalf("the final response must finish then retire the connection: %d close=%v", r.StatusCode, r.Close)
	}
}

// serveOn starts s's ListenAndServe and waits for it to listen.
func listening(t *testing.T, s *Server) (stop context.CancelFunc, done <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { ch <- s.ListenAndServe(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.lnMu.Lock()
		ln := s.ln
		s.lnMu.Unlock()
		if ln != nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the gateway never listened")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cancel, ch
}

// A restart to update hands the gateway's socket to the new magpie: a
// reply streaming through the old one when it does finishes there — the
// old one's end waits for it, rather than 2s — while new requests are
// answered by the new one, and none is refused on the way.
func TestHandedOverGatewayFinishesItsStream(t *testing.T) {
	fresh(t)
	release := make(chan struct{})
	var upstreamCalls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"quick"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"first \"}}]}\n\n")
		f.Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"last\"},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer up.Close()
	defer close(release)
	if err := provider.Save(provider.Provider{ID: "plan", Name: "Plan", Key: "upstream", Chat: up.URL + "/v1", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	// the old one's end would wait this long without the handover: far
	// shorter than the stream is held below
	oldShort := shortDrain
	shortDrain = 10 * time.Millisecond
	t.Cleanup(func() { shortDrain = oldShort })

	t.Setenv("MAGPIE_ADDR", "127.0.0.1:0")
	old := New()
	stopOld, oldDone := listening(t, old)
	defer stopOld()
	old.lnMu.Lock()
	addr := old.ln.Addr().String()
	old.lnMu.Unlock()
	t.Setenv("MAGPIE_ADDR", addr) // as settings name it, for the new one

	// a stream under way through the old one
	req, _ := http.NewRequest("POST", "http://"+addr+"/v1/chat/completions", strings.NewReader(`{"model":"plan/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Close = true
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	stream := bufio.NewReader(resp.Body)
	for {
		line, err := stream.ReadString('\n')
		if err != nil {
			t.Fatalf("the stream ended before its first chunk: %v", err)
		}
		if strings.Contains(line, "first") {
			break
		}
	}

	// the handover: the new one serves on the old one's socket
	f, err := old.ListenerFile()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.FileListener(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	acked := make(chan struct{})
	Inherit(ln, func() { close(acked) })
	next := New()
	stopNext, nextDone := listening(t, next)
	defer func() {
		stopNext()
		<-nextDone
	}()
	select {
	case <-acked:
	case <-time.After(10 * time.Second):
		t.Fatal("the new one never said it serves")
	}
	old.HandOff()
	stopOld()
	// Cancellation stops the old accept loop asynchronously. Wait until its
	// listener is closed before requiring every new request to reach next.
	// The in-flight stream must remain alive throughout this transition.
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := old.ListenerFile()
		if err != nil {
			break
		}
		f.Close()
		if time.Now().After(deadline) {
			t.Fatal("the old listener kept accepting after cancellation")
		}
		time.Sleep(time.Millisecond)
	}

	// new requests reach the new one, through the same port, while the
	// old one still streams
	for i := range 5 {
		r, err := http.Post("http://"+addr+"/v1/chat/completions", "application/json", strings.NewReader(chatReq))
		if err != nil {
			t.Fatalf("request %d during the handover: %v", i, err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != 200 || !strings.Contains(string(b), "quick") {
			t.Fatalf("request %d during the handover: %d %s", i, r.StatusCode, b)
		}
	}
	if got := len(next.Recent()); got != 5 {
		t.Fatalf("the new one answered %d of the 5 requests after the handover", got)
	}

	// the old one's end waits for the stream (well past shortDrain)
	select {
	case err := <-oldDone:
		t.Fatalf("the old one ended with a stream in flight: %v", err)
	case <-time.After(20 * shortDrain):
	}
	if b := old.Busy(); b.Requests != 1 {
		t.Fatalf("the old one has %d requests in flight, want the stream", b.Requests)
	}
	release <- struct{}{}
	rest, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("the stream was cut: %v", err)
	}
	if !strings.Contains(string(rest), "last") || !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("the stream's end didn't arrive: %q", rest)
	}
	select {
	case err := <-oldDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the old one never ended once its stream had")
	}
	if n := upstreamCalls.Load(); n != 6 {
		t.Fatalf("upstream saw %d calls, want 6", n)
	}
}

// A socket handed over for another address is closed, and the gateway
// listens where settings say.
func TestInheritedForAnotherAddressIsLetGo(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	called := false
	Inherit(ln, func() { called = true })
	got, ack := takeInherited("127.0.0.1:1")
	if got != nil || ack != nil {
		t.Fatal("took a socket on another port")
	}
	if Inherited() {
		t.Fatal("the socket stayed waiting")
	}
	if _, err := ln.Accept(); err == nil {
		t.Fatal("the socket for another address wasn't closed")
	}
	if called {
		t.Fatal("acked a socket not served")
	}
}

func TestSameAddr(t *testing.T) {
	for _, tc := range []struct {
		ln   string
		addr string
		want bool
	}{
		{"127.0.0.1:3425", "127.0.0.1:3425", true},
		{"127.0.0.1:3425", "localhost:3425", true},
		{"127.0.0.1:3425", "127.0.0.1:3426", false},
		{"[::]:3425", "0.0.0.0:3425", true},
		{"0.0.0.0:3425", ":3425", true},
		{"0.0.0.0:3425", "127.0.0.1:3425", false},
		{"127.0.0.1:3425", "0.0.0.0:3425", false},
	} {
		a, err := net.ResolveTCPAddr("tcp", tc.ln)
		if err != nil {
			t.Fatal(err)
		}
		if got := sameAddr(a, tc.addr); got != tc.want {
			t.Errorf("sameAddr(%s, %s) = %v, want %v", tc.ln, tc.addr, got, tc.want)
		}
	}
}

// A Claude Code run of the magpie before this one, its turn under way when
// the gateway changed hands, calls its tools on the port this one now
// serves: the call goes on to that magpie's callbacks socket, through the
// network guard as its own run's would, and comes back answered. Once that
// magpie is gone, such a call is unknown here again, and guarded.
func TestToolCallOfTheMagpieBeforeGoesToIt(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_ADDR", "0.0.0.0:3425")
	t.Setenv("MAGPIE_TRUST_PROXY", "")
	newCaller(t, "Host") // gateway keys on
	sock := shortSock(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var got atomic.Value
	before := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.Store(r.Method + " " + r.URL.Path + " " + string(b))
		io.WriteString(w, `{"content":"answered there"}`)
	})}
	go before.Serve(ln)
	defer before.Close()
	ForwardCallbacks(sock)
	t.Cleanup(func() { ForwardCallbacks("") })

	h := lanGuard(New().Handler())
	token := randomToken()
	call := func(remote string) (int, string) {
		r := httptest.NewRequest("POST", claudeMCPPath+token, strings.NewReader(`{"tool_call_id":"x","name":"read","arguments":{}}`))
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	// from this computer's own LAN address, as on a non-loopback MAGPIE_ADDR
	// (#1546): no key, as for this one's own runs
	if c, b := call("192.168.1.9:5000"); c != 200 || !strings.Contains(b, "answered there") {
		t.Fatalf("the call wasn't passed on: %d %s", c, b)
	}
	if g, _ := got.Load().(string); !strings.HasPrefix(g, "POST "+claudeMCPPath+token+" ") || !strings.Contains(g, `"tool_call_id":"x"`) {
		t.Fatalf("the magpie before got %q", g)
	}
	// that magpie gone: the call is unknown here, and from the network it
	// needs a key again
	before.Close()
	os.Remove(sock)
	if c, b := call("127.0.0.1:5000"); c != http.StatusNotFound {
		t.Fatalf("with the magpie before gone: %d %s", c, b)
	}
	if forwarding() {
		t.Fatal("still passing calls on to a magpie gone")
	}
	if c, b := call("192.168.1.9:5000"); c != http.StatusUnauthorized {
		t.Fatalf("from the network, with the magpie before gone: %d %s", c, b)
	}
}

// The callbacks socket answers its own runs' tool calls and nothing else.
func TestCallbacksSocketServesOnlyToolCalls(t *testing.T) {
	fresh(t)
	s := New()
	sock := shortSock(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go s.ServeCallbacks(ln)
	defer ln.Close()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	for _, p := range [][2]string{
		{"POST", claudeMCPPath + randomToken()},
		{"POST", "/v1/chat/completions"},
		{"GET", "/v1/models"},
		{"GET", claudeMCPPath + randomToken()},
	} {
		req, _ := http.NewRequest(p[0], "http://x"+p[1], strings.NewReader(chatReq))
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode == 200 {
			t.Errorf("%s %s answered 200", p[0], p[1])
		}
	}
}

// A bridge turn counts while it runs, and its end is noted, for a restart
// to wait on: those are what a handover can't carry.
func TestBridgeTurnsCount(t *testing.T) {
	s := New()
	done := s.bridgeTurn()
	if b := s.Busy(); b.Bridge != 1 {
		t.Fatalf("Bridge = %d during a turn", b.Bridge)
	}
	before := time.Now()
	done()
	b := s.Busy()
	if b.Bridge != 0 || b.BridgeLast.Before(before) {
		t.Fatalf("after the turn: %+v", b)
	}
}

// shortSock is a unix socket path short enough for a Mac (104 bytes),
// which a test's TempDir may not be.
func shortSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mho")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "cb")
}
