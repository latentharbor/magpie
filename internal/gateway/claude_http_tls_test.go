package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/netproxy"
)

type claudeHelloObservation struct {
	RecordVersion uint16                 `json:"recordVersion"`
	HelloVersion  uint16                 `json:"helloVersion"`
	RecordLength  int                    `json:"recordLength"`
	SessionLength int                    `json:"sessionLength"`
	Ciphers       string                 `json:"ciphers"`
	Compression   string                 `json:"compression"`
	Extensions    []claudeHelloExtension `json:"extensions"`
}
type claudeHelloExtension struct {
	Type uint16 `json:"type"`
	Data string `json:"data"`
}

// Parse bytes actually written to a socket, not the preset's own declarations.
// Ephemeral values are deliberately excluded; group IDs and sizes are not.
func observeClaudeHello(t *testing.T, record []byte) claudeHelloObservation {
	t.Helper()
	r := bytes.NewReader(record)
	u8 := func() byte {
		b, err := r.ReadByte()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	u16 := func() uint16 {
		var n uint16
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	take := func(n int) []byte {
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	if u8() != 22 {
		t.Fatal("not a TLS handshake record")
	}
	o := claudeHelloObservation{RecordVersion: u16(), RecordLength: len(record)}
	if int(u16()) != r.Len() {
		t.Fatal("bad TLS record length")
	}
	if u8() != 1 {
		t.Fatal("not ClientHello")
	}
	take(3)
	o.HelloVersion = u16()
	take(32)
	o.SessionLength = int(u8())
	take(o.SessionLength)
	o.Ciphers = hex.EncodeToString(take(int(u16())))
	o.Compression = hex.EncodeToString(take(int(u8())))
	if int(u16()) != r.Len() {
		t.Fatal("bad extensions length")
	}
	for r.Len() > 0 {
		typ, size := u16(), int(u16())
		data := take(size)
		value := hex.EncodeToString(data)
		if typ == 51 {
			if len(data) < 2 {
				t.Fatal("bad key share")
			}
			var parts []string
			for pos := 2; pos < len(data); {
				if pos+4 > len(data) {
					t.Fatal("short key share")
				}
				group, n := binary.BigEndian.Uint16(data[pos:]), int(binary.BigEndian.Uint16(data[pos+2:]))
				pos += 4
				if pos+n > len(data) {
					t.Fatal("short key")
				}
				parts = append(parts, fmt.Sprintf("%d:%d", group, n))
				pos += n
			}
			value = strings.Join(parts, ",")
		}
		o.Extensions = append(o.Extensions, claudeHelloExtension{typ, value})
	}
	return o
}

func readClaudeHello(conn net.Conn) ([]byte, error) {
	head := make([]byte, 5)
	if _, err := io.ReadFull(conn, head); err != nil {
		return nil, err
	}
	body := make([]byte, int(binary.BigEndian.Uint16(head[3:])))
	_, err := io.ReadFull(conn, body)
	return append(head, body...), err
}

func TestClaudeHTTPHelloMatchesNative309(t *testing.T) {
	wantBytes, err := os.ReadFile("testdata/claude-2.1.309-darwin-arm64-hello.json")
	if err != nil {
		t.Fatal(err)
	}
	var want claudeHelloObservation
	if err = json.Unmarshal(wantBytes, &want); err != nil {
		t.Fatal(err)
	}
	var previous []byte
	for range 2 {
		client, server := net.Pipe()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		done := make(chan error, 1)
		go func() {
			_, err := handshakeClaudeHTTP(ctx, client, "api.anthropic.com", nil)
			done <- err
		}()
		record, readErr := readClaudeHello(server)
		server.Close()
		cancel()
		<-done
		if readErr != nil {
			t.Fatal(readErr)
		}
		got := observeClaudeHello(t, record)
		if !reflect.DeepEqual(got, want) {
			b, _ := json.MarshalIndent(got, "", "  ")
			t.Fatalf("native ClientHello differs:\n%s", b)
		}
		if previous != nil && bytes.Equal(previous, record) {
			t.Fatal("reused ephemeral handshake bytes")
		}
		previous = record
	}
}

func TestClaudeHTTPTLSReusesAndReconnectsLikeNative(t *testing.T) {
	var connections atomic.Int32
	resumed := make(chan bool, 4)
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 || r.TLS.NegotiatedProtocol != "http/1.1" {
			t.Error("not native HTTP/1.1 ALPN")
		}
		resumed <- r.TLS.DidResume
		io.WriteString(w, "OK")
	}))
	s.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	s.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	s.EnableHTTP2 = true
	s.StartTLS()
	defer s.Close()
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	transport := newClaudeHTTPPool(nil, roots)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	get := func() {
		t.Helper()
		res, err := client.Get(s.URL)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil || string(b) != "OK" {
			t.Fatalf("bad reply %q %v", b, err)
		}
	}
	get()
	if <-resumed {
		t.Fatal("first handshake resumed")
	}
	get()
	<-resumed
	if connections.Load() != 1 {
		t.Fatal("did not reuse HTTP connection")
	}
	transport.CloseIdleConnections()
	get()
	if <-resumed {
		t.Fatal("introduced TLS resumption absent in the native reconnect capture")
	}
	if connections.Load() != 2 {
		t.Fatal("wrong reconnect count")
	}
	// The custom handshake still verifies certificates; there is no insecure
	// fallback to Go's default transport on a verification error.
	untrusted := newClaudeHTTPPool(nil, x509.NewCertPool())
	defer untrusted.CloseIdleConnections()
	if res, err := (&http.Client{Transport: untrusted, Timeout: 10 * time.Second}).Get(s.URL); err == nil {
		res.Body.Close()
		t.Fatal("accepted an untrusted server")
	}
}

func TestClaudeHTTPTLSConnectUsesNativeHello(t *testing.T) {
	captured := make(chan []byte, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != "api.anthropic.com:443" {
			t.Error("wrong CONNECT destination")
			w.WriteHeader(400)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		rw.Flush()
		record, err := readClaudeHello(conn)
		if err != nil {
			t.Error(err)
			return
		}
		captured <- record
	}))
	defer proxy.Close()
	fallback := false
	wire := &claudeHTTPWire{pools: map[string]*http.Transport{}, next: claudeHTTPRoundTrip(func(*http.Request) (*http.Response, error) {
		fallback = true
		return nil, fmt.Errorf("unexpected fallback")
	})}
	defer wire.CloseIdleConnections()
	ctx := context.WithValue(netproxy.With(context.Background(), proxy.URL), claudeHTTPKey{}, claudeHTTPCall{})
	r, _ := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", strings.NewReader(`{}`))
	if _, err := wire.RoundTrip(r); err == nil {
		t.Fatal("sink should end the handshake")
	}
	if fallback {
		t.Fatal("Claude TLS silently fell back to ordinary transport")
	}
	select {
	case record := <-captured:
		got := observeClaudeHello(t, record)
		if got.RecordLength != 1508 || got.Extensions[3].Data != "000811ec001d00170018" {
			t.Fatal("CONNECT used a different TLS stack")
		}
	default:
		t.Fatal("proxy saw no ClientHello")
	}
	r.URL, _ = url.Parse("https://elsewhere.invalid/v1/messages")
	if _, err := wire.RoundTrip(r); err == nil {
		t.Fatal("OAuth transport accepted a different origin")
	}
	// A non-Claude request is still handled by the original transport.
	r = r.WithContext(context.Background())
	wire.RoundTrip(r)
	if !fallback {
		t.Fatal("ordinary vendor was routed through Claude TLS")
	}
}

func TestClaudeHTTPConnectCancellation(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	arrived := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		http.ReadRequest(bufio.NewReader(conn))
		close(arrived)
		io.Copy(io.Discard, conn)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, _ := url.Parse("http://" + ln.Addr().String())
	done := make(chan error, 1)
	go func() { _, err := dialClaudeHTTP(ctx, "tcp", "api.anthropic.com:443", p); done <- err }()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("CONNECT not received")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled CONNECT succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not unblock CONNECT")
	}
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("CONNECT socket leaked")
	}
}
