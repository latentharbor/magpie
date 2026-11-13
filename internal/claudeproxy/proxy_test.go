package claudeproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/claudelogin"
)

func fixtureConfig(t *testing.T, upstreamURL string) (string,Config) {
	t.Helper();dir:=t.TempDir();key:=filepath.Join(dir,"key")
	if err:=os.WriteFile(key,[]byte("device-fixture-key\n"),0o600);err!=nil{t.Fatal(err)}
	ln,err:=net.Listen("tcp","127.0.0.1:0");if err!=nil{t.Fatal(err)};listen:=ln.Addr().String();ln.Close()
	c:=Config{Schema:1,Enabled:true,Listen:listen,Models:map[string]string{"claude-opus-5-5":"nas/team/opus"},GatewayUsage:true,APICertDir:"api",Upstreams:map[string]Upstream{"nas":{URL:upstreamURL,KeyFile:"key"}},ImageUpstream:"nas"}
	p:=filepath.Join(dir,"config.json");b,_:=json.Marshal(c);if err:=os.WriteFile(p,b,0o600);err!=nil{t.Fatal(err)};return p,c
}

func TestStandaloneProxyUsesOnlyRemoteInferenceAndLocalUsage(t *testing.T) {
	called:=make(chan string,1)
	nas:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		b,_:=io.ReadAll(r.Body)
		if r.Header.Get("Authorization")!="Bearer device-fixture-key"||r.Header.Get("Cookie")!=""||!bytes.Contains(b,[]byte(`"model":"team/opus"`)){t.Error("native credential or model boundary changed")}
		called<-r.URL.Path;w.Header().Set("Content-Type","application/json");w.Header().Set("Request-Id","req_fixture");io.WriteString(w,`{"type":"message","content":[]}`)
	}));defer nas.Close()
	p,c:=fixtureConfig(t,nas.URL)
	a,err:=claudelogin.EnsureAuthority(context.Background(),resolvePath(p,c.APICertDir));if err!=nil{t.Fatal(err)}
	ctx,cancel:=context.WithCancel(context.Background());done:=make(chan error,1)
	go func(){done<-run(ctx,p,"fixture",func(string,string)claudelogin.CertificateStatus{return claudelogin.CertificateStatus{Trusted:true}})}()
	t.Cleanup(func(){cancel();select{case err:=<-done:if err!=nil{t.Error(err)};case <-time.After(5*time.Second):t.Error("daemon did not stop")}})
	for deadline:=time.Now().Add(5*time.Second);;{
		if _,err:=Status(context.Background(),p);err==nil{break};if time.Now().After(deadline){t.Fatal("status socket did not open")};time.Sleep(10*time.Millisecond)
	}
	proxy,_:=url.Parse("http://"+c.Listen);roots:=x509.NewCertPool();roots.AddCert(a.CA)
	transport:=&http.Transport{Proxy:http.ProxyURL(proxy),TLSClientConfig:&tls.Config{RootCAs:roots}};defer transport.CloseIdleConnections()
	client:=&http.Client{Transport:transport,Timeout:5*time.Second}
	var response *http.Response
	for deadline:=time.Now().Add(5*time.Second);;{
		response,err=client.Get("https://api.anthropic.com/api/oauth/usage");if err==nil{break};if time.Now().After(deadline){t.Fatal(err)};time.Sleep(10*time.Millisecond)
	}
	body,_:=io.ReadAll(response.Body);response.Body.Close();if !bytes.Contains(body,[]byte("magpie_gateway_managed")){t.Fatal("local quota response missing")}
	r,_:=http.NewRequest("POST","https://api.anthropic.com/v1/messages",strings.NewReader(`{"model":"claude-opus-5-5","thread":{"type":"continue","previous_message_id":"m"},"messages":[]}`));r.Header.Set("Authorization","Bearer official-secret");r.Header.Set("Cookie","official=secret")
	response,err=client.Do(r);if err!=nil{t.Fatal(err)};io.Copy(io.Discard,response.Body);response.Body.Close()
	if response.StatusCode!=200||<-called!="/v1/messages"{t.Fatal("request did not reach NAS")}
}

func TestStandaloneDrainRejectsNewCallsAndWaitsForBody(t *testing.T) {
	jobs:=&activity{}
	end,err:=jobs.begin();if err!=nil{t.Fatal(err)}
	done:=make(chan error,1);go func(){done<-jobs.drain(context.Background())}()
	for deadline:=time.Now().Add(time.Second);;{jobs.mu.Lock();draining:=jobs.draining;jobs.mu.Unlock();if draining{break};if time.Now().After(deadline){t.Fatal("drain did not start")};time.Sleep(time.Millisecond)}
	if _,err:=jobs.begin();err==nil{t.Fatal("new call entered while draining")}
	select{case <-done:t.Fatal("drain ended with a live stream");default:}
	end();end();if err:=<-done;err!=nil||jobs.count()!=0{t.Fatal("body completion did not release ownership")}
}

func TestStandaloneImportDoesNotCopyAccountsOrChangeSources(t *testing.T) {
	from:=t.TempDir();destination:=filepath.Join(t.TempDir(),"proxy","config.json")
	login:=claudelogin.Config{Enabled:true,Listen:"127.0.0.1:3426",Models:map[string]string{"claude-opus":"nas/team/opus"}}
	b,_:=json.Marshal(login);os.WriteFile(filepath.Join(from,"claude-login.json"),b,0o600)
	os.WriteFile(filepath.Join(from,"providers.json"),[]byte(`{"providers":[{"id":"nas","preset":"remote-magpie","key":"device-fixture-key","anthropic":"http://192.168.1.2:3425"}]}`),0o600)
	os.WriteFile(filepath.Join(from,"logins.json"),[]byte(`{"never":"copy subscription credentials"}`),0o600)
	a,err:=claudelogin.EnsureAuthority(context.Background(),filepath.Join(from,"claude-login-ca"));if err!=nil{t.Fatal(err)}
	if err:=ImportMagpie(from,destination);err!=nil{t.Fatal(err)}
	c,err:=Load(destination);if err!=nil||c.Models["claude-opus"]!="nas/team/opus"{t.Fatal("mappings lost")}
	cert,err:=claudelogin.EnsureAuthority(context.Background(),resolvePath(destination,c.APICertDir));if err!=nil||!bytes.Equal(cert.CA.Raw,a.CA.Raw){t.Fatal("import changed the existing CA")}
	if _,err:=os.Stat(filepath.Join(filepath.Dir(destination),"logins.json"));!os.IsNotExist(err){t.Fatal("account store was copied")}
	if err:=ImportMagpie(from,destination);err==nil{t.Fatal("existing configuration overwritten")}
	if st,_:=os.Stat(filepath.Join(filepath.Dir(destination),"keys","nas"));st.Mode().Perm()!=0o600{t.Fatal("key permissions are not private")}
}

func TestStandaloneMCPForwardsSessionAndSavesImages(t *testing.T) {
	const png="iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	nas:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		if r.URL.Path!=imageEndpoint||r.Header.Get("Authorization")!="Bearer device-fixture-key"{t.Error("MCP request credentials or path changed")}
		var m rpc;json.NewDecoder(r.Body).Decode(&m);w.Header().Set("Content-Type","application/json")
		var result any=map[string]any{}
		switch m.Method {
		case "initialize":w.Header().Set("Mcp-Session-Id","fixture-session");result=map[string]any{"protocolVersion":"2025-06-18","capabilities":map[string]any{"tools":map[string]any{}},"serverInfo":map[string]any{"name":"image","version":"1"}}
		case "tools/list":if r.Header.Get("Mcp-Session-Id")!="fixture-session"{t.Error("MCP session lost")};result=map[string]any{"tools":[]any{map[string]any{"name":"generate_image","inputSchema":map[string]any{"properties":map[string]any{"reference_images":map[string]any{}}}}}}
		case "tools/call":
			var p struct{Arguments map[string]any `json:"arguments"`};json.Unmarshal(m.Params,&p)
			if p.Arguments["path"]!=nil{t.Error("local destination leaked to NAS")}
			refs,_:=p.Arguments["reference_images"].([]any);if len(refs)!=1||!strings.HasPrefix(refs[0].(string),"data:image/png;base64,"){t.Error("local reference was not encoded")}
			result=map[string]any{"content":[]any{map[string]any{"type":"image","mimeType":"image/png","data":png}}}
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc":"2.0","id":m.ID,"result":result})
	}));defer nas.Close()
	p,_:=fixtureConfig(t,nas.URL);var out bytes.Buffer
	b:=&imageBridge{config:p,client:&http.Client{Timeout:5*time.Second},out:json.NewEncoder(&out),paths:map[string]string{},cancel:map[string]context.CancelFunc{}}
	call:=func(id,method string,params any){t.Helper();raw,_:=json.Marshal(map[string]any{"jsonrpc":"2.0","id":json.RawMessage(id),"method":method,"params":params});var m rpc;json.Unmarshal(raw,&m);if err:=b.forward(context.Background(),m,raw);err!=nil{t.Fatal(err)}}
	call("1","initialize",map[string]any{});call("2","tools/list",map[string]any{})
	data,_:=base64.StdEncoding.DecodeString(png);ref:=filepath.Join(t.TempDir(),"reference.png");os.WriteFile(ref,data,0o600)
	destination:=filepath.Join(t.TempDir(),"result.png");os.WriteFile(destination,[]byte("keep me"),0o600)
	call("9007199254740993","tools/call",map[string]any{"name":"generate_image","arguments":map[string]any{"prompt":"fixture","path":destination,"reference_images":[]string{ref}}})
	if got,_:=os.ReadFile(destination);string(got)!="keep me"{t.Fatal("existing file was overwritten")}
	if got,_:=os.ReadFile(strings.TrimSuffix(destination,".png")+"-1.png");!bytes.Equal(got,data){t.Fatal("generated image not saved locally")}
	if !strings.Contains(out.String(),`"id":9007199254740993`){t.Fatal("MCP request ID lost precision")}
}

func TestStandaloneSSEKeepsSeparateMessages(t *testing.T) {
	var mu sync.Mutex;var messages []string
	err:=readSSE(strings.NewReader(": keepalive\n\nevent: message\ndata: {\"method\":\"notifications/progress\"}\n\ndata: {\"id\":1,\"result\":{}}\n\n"),func(b []byte)error{mu.Lock();messages=append(messages,string(b));mu.Unlock();return nil})
	if err!=nil||len(messages)!=2{t.Fatalf("SSE messages lost: %v %v",messages,err)}
}
