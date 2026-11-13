package claudeproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const imageEndpoint = "/mcp/magpie/image"
const rootsID = "claude-login-proxy/roots"

type rpc struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type imageBridge struct {
	config                  string
	client                  *http.Client
	out                     *json.Encoder
	outMu                   sync.Mutex
	mu                      sync.Mutex
	session, protocol, root string
	roots                   bool
	paths                   map[string]string
	cancel                  map[string]context.CancelFunc
}

// ImageMCP translates only the transport and local file arguments. Generation,
// models and pending jobs remain on the NAS's authenticated image MCP server.
func ImageMCP(ctx context.Context, config string, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan struct{})
	defer close(finished)
	if closer, ok := in.(io.Closer); ok {
		go func() {
			select {
			case <-ctx.Done():
				closer.Close()
				if writer, ok := out.(io.Closer); ok {
					writer.Close()
				}
			case <-finished:
			}
		}()
	}
	transport := &http.Transport{ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	b := &imageBridge{config: config, out: json.NewEncoder(out), paths: map[string]string{}, cancel: map[string]context.CancelFunc{},
		client: &http.Client{Transport: transport, Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	err := b.serve(ctx, in)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (b *imageBridge) send(v any) { b.outMu.Lock(); defer b.outMu.Unlock(); _ = b.out.Encode(v) }
func (b *imageBridge) fail(id json.RawMessage, message string) {
	if len(id) == 0 {
		return
	}
	b.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32000, "message": message}})
}

func (b *imageBridge) serve(parent context.Context, in io.Reader) error {
	ctx, stop := context.WithCancel(parent)
	defer stop()
	var workers sync.WaitGroup
	scan := bufio.NewScanner(in)
	scan.Buffer(make([]byte, 64<<10), 64<<20)
	for scan.Scan() {
		line := bytes.Clone(scan.Bytes())
		var m rpc
		if json.Unmarshal(line, &m) != nil || m.JSONRPC != "2.0" {
			b.fail(json.RawMessage("null"), "invalid JSON-RPC message")
			continue
		}
		if string(m.ID) == `"`+rootsID+`"` && m.Method == "" {
			var result struct {
				Roots []struct {
					URI string `json:"uri"`
				} `json:"roots"`
			}
			if json.Unmarshal(m.Result, &result) == nil {
				for _, r := range result.Roots {
					u, e := url.Parse(r.URI)
					if e == nil && u.Scheme == "file" && (u.Host == "" || u.Host == "localhost") && filepath.IsAbs(u.Path) {
						b.mu.Lock()
						b.root = u.Path
						b.mu.Unlock()
						break
					}
				}
			}
			continue
		}
		if m.Method == "initialize" {
			var p struct {
				Capabilities map[string]json.RawMessage `json:"capabilities"`
			}
			_ = json.Unmarshal(m.Params, &p)
			b.mu.Lock()
			b.roots = p.Capabilities["roots"] != nil
			b.mu.Unlock()
		}
		if m.Method == "notifications/initialized" {
			b.mu.Lock()
			roots := b.roots
			b.roots = false
			b.mu.Unlock()
			if roots {
				b.send(map[string]any{"jsonrpc": "2.0", "id": rootsID, "method": "roots/list"})
			}
		}
		if m.Method == "notifications/cancelled" {
			var p struct {
				ID json.RawMessage `json:"requestId"`
			}
			_ = json.Unmarshal(m.Params, &p)
			b.mu.Lock()
			f := b.cancel[string(p.ID)]
			b.mu.Unlock()
			if f != nil {
				f()
			}
		}
		call, cancel := context.WithCancel(ctx)
		key := string(m.ID)
		b.mu.Lock()
		_, duplicate := b.cancel[key]
		if key != "" && !duplicate {
			b.cancel[key] = cancel
		}
		b.mu.Unlock()
		if key != "" && duplicate {
			cancel()
			b.fail(m.ID, "duplicate active request ID")
			continue
		}
		workers.Add(1)
		go func(m rpc, raw []byte, key string, cancel context.CancelFunc) {
			defer workers.Done()
			defer cancel()
			defer func() {
				if key != "" {
					b.mu.Lock()
					delete(b.cancel, key)
					b.mu.Unlock()
				}
			}()
			if err := b.forward(call, m, raw); err != nil {
				b.fail(m.ID, err.Error())
			}
		}(m, line, key, cancel)
	}
	stop()
	workers.Wait()
	return scan.Err()
}

func (b *imageBridge) forward(ctx context.Context, m rpc, raw []byte) error {
	c, err := Load(b.config)
	if err != nil {
		return fmt.Errorf("image proxy configuration unavailable")
	}
	u, key, err := upstream(b.config, c, c.ImageUpstream)
	if err != nil {
		return fmt.Errorf("image gateway unavailable")
	}
	save := ""
	if m.Method == "tools/call" {
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			return fmt.Errorf("invalid image tool arguments")
		}
		if p.Name == "generate_image" {
			save, _ = p.Arguments["path"].(string)
			if save == "" {
				save = filepath.Join("generated-images", fmt.Sprintf("image-%d", time.Now().UnixNano()))
			}
			delete(p.Arguments, "path")
			if refs, ok := p.Arguments["reference_images"].([]any); ok {
				for i, v := range refs {
					s, ok := v.(string)
					if !ok {
						return fmt.Errorf("reference image must be a path or URL")
					}
					s, err = b.imageInput(s)
					if err != nil {
						return err
					}
					refs[i] = s
				}
			}
		} else if p.Name == "generation_result" {
			id, _ := p.Arguments["id"].(string)
			b.mu.Lock()
			save = b.paths[id]
			b.mu.Unlock()
		}
		if save != "" {
			save = b.localPath(save)
		}
		// Retain progress tokens and other MCP metadata outside arguments.
		var params map[string]json.RawMessage
		_ = json.Unmarshal(m.Params, &params)
		params["arguments"], _ = json.Marshal(p.Arguments)
		m.Params, _ = json.Marshal(params)
		raw, _ = json.Marshal(m)
	}
	if len(raw) > 64<<20 {
		return fmt.Errorf("image request is too large")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(u.String(), "/")+imageEndpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("invalid image request")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	b.mu.Lock()
	session, protocol := b.session, b.protocol
	b.mu.Unlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if protocol != "" {
		req.Header.Set("MCP-Protocol-Version", protocol)
	}
	res, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("image gateway request failed")
	}
	defer res.Body.Close()
	if res.StatusCode == 202 || res.StatusCode == 204 {
		return nil
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("image gateway returned HTTP %d", res.StatusCode)
	}
	if id := res.Header.Get("Mcp-Session-Id"); id != "" {
		b.mu.Lock()
		b.session = id
		b.mu.Unlock()
	}
	emit := func(data []byte) error {
		var response map[string]any
		if json.Unmarshal(data, &response) != nil {
			return fmt.Errorf("invalid MCP response")
		}
		var envelope struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(data, &envelope)
		if len(envelope.ID) > 0 {
			response["id"] = envelope.ID
		}
		if result, ok := response["result"].(map[string]any); ok {
			if m.Method == "initialize" {
				if p, ok := result["protocolVersion"].(string); ok {
					b.mu.Lock()
					b.protocol = p
					b.mu.Unlock()
				}
			}
			if m.Method == "tools/list" {
				augmentImageTools(result)
			}
			if save != "" && m.Method == "tools/call" {
				if meta, ok := result["_meta"].(map[string]any); ok {
					if id, ok := meta["usemagpie.ai/pending"].(string); ok {
						b.mu.Lock()
						if len(b.paths) >= 1024 {
							clear(b.paths)
						}
						b.paths[id] = save
						b.mu.Unlock()
					}
				}
				if err := b.saveImages(ctx, u, key, save, result); err != nil {
					content, _ := result["content"].([]any)
					result["content"] = append(content, map[string]any{"type": "text", "text": "The image was generated, but local saving failed. Use the returned image or URL; do not generate it again. " + err.Error()})
				}
			}
		}
		b.send(response)
		return nil
	}
	reader := io.LimitReader(res.Body, 64<<20+1)
	if strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
		return readSSE(reader, emit)
	}
	data, err := io.ReadAll(reader)
	if err != nil || len(data) > 64<<20 {
		return fmt.Errorf("image response is too large or unreadable")
	}
	return emit(data)
}

func readSSE(r io.Reader, emit func([]byte) error) error {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64<<10), 64<<20)
	var data []string
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		b := []byte(strings.Join(data, "\n"))
		data = nil
		return emit(b)
	}
	for s.Scan() {
		line := s.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := s.Err(); err != nil {
		return fmt.Errorf("MCP stream interrupted")
	}
	return flush()
}

func augmentImageTools(result map[string]any) {
	tools, _ := result["tools"].([]any)
	for _, entry := range tools {
		t, ok := entry.(map[string]any)
		if !ok || t["name"] != "generate_image" {
			continue
		}
		schema, _ := t["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if props == nil {
			continue
		}
		props["path"] = map[string]any{"type": "string", "description": "Optional local image file or folder to save the result. Existing files are never overwritten."}
		if refs, ok := props["reference_images"].(map[string]any); ok {
			refs["description"] = "Local image file paths, HTTP(S) image URLs or data URLs; local files are sent to the configured NAS."
		}
	}
}

func (b *imageBridge) localPath(name string) string {
	if filepath.IsAbs(name) {
		return filepath.Clean(name)
	}
	b.mu.Lock()
	root := b.root
	b.mu.Unlock()
	if root == "" {
		root, _ = os.Getwd()
		if root == "/" || root == "" {
			home, _ := os.UserHomeDir()
			root = filepath.Join(home, "Pictures", "Claude Proxy")
		}
	}
	return filepath.Join(root, name)
}

func (b *imageBridge) imageInput(s string) (string, error) {
	if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "data:image/") {
		return s, nil
	}
	f, err := os.Open(b.localPath(s))
	if err != nil {
		return "", fmt.Errorf("reference image cannot be opened")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
	if err != nil || len(data) > 16<<20 {
		return "", fmt.Errorf("reference image exceeds 16 MiB or cannot be read")
	}
	mt := http.DetectContentType(data)
	if !strings.HasPrefix(mt, "image/") {
		return "", fmt.Errorf("reference file is not an image")
	}
	return "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func (b *imageBridge) saveImages(ctx context.Context, upstream *url.URL, key, destination string, result map[string]any) error {
	content, _ := result["content"].([]any)
	type picture struct {
		data []byte
		mime string
	}
	var images []picture
	for _, entry := range content {
		v, ok := entry.(map[string]any)
		if !ok || v["type"] != "image" {
			continue
		}
		s, _ := v["data"].(string)
		mt, _ := v["mimeType"].(string)
		data, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(data) > 32<<20 {
			return fmt.Errorf("invalid or oversized image")
		}
		images = append(images, picture{data, mt})
	}
	if len(images) == 0 {
		meta, _ := result["_meta"].(map[string]any)
		files, _ := meta["usemagpie.ai/files"].([]any)
		for _, entry := range files {
			f, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			raw, _ := f["url"].(string)
			u, err := url.Parse(raw)
			if err != nil || !strings.HasPrefix(path.Clean(u.Path), imageEndpoint+"/files/") {
				continue
			}
			// NAS public URLs may name its LAN IP; use the authenticated route
			// selected on this client (including its VPN), never a response's host.
			u.Scheme, u.Host, u.User = upstream.Scheme, upstream.Host, nil
			req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
			req.Header.Set("Authorization", "Bearer "+key)
			res, err := b.client.Do(req)
			if err != nil {
				return fmt.Errorf("generated image download failed")
			}
			data, readErr := io.ReadAll(io.LimitReader(res.Body, 32<<20+1))
			res.Body.Close()
			if res.StatusCode != 200 || readErr != nil || len(data) > 32<<20 {
				return fmt.Errorf("generated image download failed")
			}
			mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
			images = append(images, picture{data, mt})
		}
	}
	for i, img := range images {
		ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif"}[img.mime]
		if ext == "" || !strings.HasPrefix(http.DetectContentType(img.data), "image/") {
			return fmt.Errorf("generated file is not a supported raster image")
		}
		p := destination
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			p = filepath.Join(p, "generated"+ext)
		} else if filepath.Ext(p) == "" {
			p += ext
		}
		if len(images) > 1 {
			p = strings.TrimSuffix(p, filepath.Ext(p)) + fmt.Sprintf("-%d", i+1) + filepath.Ext(p)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return fmt.Errorf("cannot create image destination")
		}
		var file *os.File
		base := p
		for n := 0; n < 100; n++ {
			if n > 0 {
				p = strings.TrimSuffix(base, filepath.Ext(base)) + fmt.Sprintf("-%d", n) + filepath.Ext(base)
			}
			var err error
			file, err = os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err == nil {
				break
			}
			if !os.IsExist(err) {
				return fmt.Errorf("cannot save image")
			}
		}
		if file == nil {
			return fmt.Errorf("image destination names are already in use")
		}
		_, err := file.Write(img.data)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			return fmt.Errorf("image could not be completely saved")
		}
		content = append(content, map[string]any{"type": "text", "text": "Saved image: " + p})
	}
	result["content"] = content
	return nil
}
