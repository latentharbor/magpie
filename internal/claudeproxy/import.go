package claudeproxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yetone/magpie/internal/claudelogin"
)

// ImportMagpie copies only the selected remote key, mappings and already-owned
// certificates. It never copies subscription accounts or changes system trust.
func ImportMagpie(from, destination string) error {
	if from=="" {home,_:=os.UserHomeDir();from=filepath.Join(home,".config","magpie")}
	if _,err:=os.Stat(destination);!os.IsNotExist(err){return fmt.Errorf("destination already exists or cannot be inspected")}
	b,err:=os.ReadFile(filepath.Join(from,"claude-login.json"));if err!=nil{return err}
	var login claudelogin.Config
	if json.Unmarshal(b,&login)!=nil{return fmt.Errorf("invalid existing login proxy configuration")}
	if err:=login.Validate();err!=nil{return err}
	b,err=os.ReadFile(filepath.Join(from,"providers.json"));if err!=nil{return err}
	var providers struct{Providers []struct{ID, Preset, Key, Anthropic string;Off bool} `json:"providers"`}
	if json.Unmarshal(b,&providers)!=nil{return fmt.Errorf("invalid existing provider configuration")}
	c:=Config{Schema:1,Enabled:login.Enabled,Listen:login.Listen,Models:login.Models,GatewayUsage:login.GatewayUsage,UsageOrganization:login.UsageOrganization,APICertDir:"certs/api",WebCertDir:"certs/web",Upstreams:map[string]Upstream{},MediaListen:"127.0.0.1:3425"}
	keys:=map[string]string{}
	for _,target:=range login.Models {
		id,_,_:=strings.Cut(target,"/")
		valid:=id!=""
		for _,r:=range id{valid=valid&&((r>='a'&&r<='z')||(r>='A'&&r<='Z')||(r>='0'&&r<='9')||r=='-'||r=='_')}
		if !valid{return fmt.Errorf("invalid upstream identifier")}
		for _,p:=range providers.Providers {
			if p.ID==id&&p.Preset=="remote-magpie"&&!p.Off&&p.Key!="" {
				if _,err:=endpoint(p.Anthropic);err!=nil{return err}
				c.Upstreams[id]=Upstream{URL:p.Anthropic,KeyFile:filepath.Join("keys",id)};keys[id]=p.Key
			}
		}
		if c.Upstreams[id].URL==""{return fmt.Errorf("all mappings must use an enabled Remote magpie provider")}
		if c.ImageUpstream==""{c.ImageUpstream=id}
	}
	if len(c.Upstreams)!=1{return fmt.Errorf("choose an image upstream explicitly for multiple remote gateways")}
	targetRoot:=filepath.Dir(destination)
	if entries,err:=os.ReadDir(targetRoot);err==nil&&len(entries)!=0{return fmt.Errorf("destination directory is not empty")}else if err!=nil&&!os.IsNotExist(err){return err}
	if err:=os.MkdirAll(filepath.Dir(targetRoot),0o700);err!=nil{return err}
	root,err:=os.MkdirTemp(filepath.Dir(targetRoot),".claude-proxy-import-");if err!=nil{return err};defer os.RemoveAll(root)
	write:=func(p string,data []byte)error{if err:=os.MkdirAll(filepath.Dir(p),0o700);err!=nil{return err};f,err:=os.OpenFile(p,os.O_CREATE|os.O_EXCL|os.O_WRONLY,0o600);if err!=nil{return err};_,err=f.Write(data);closeErr:=f.Close();if err!=nil{return err};return closeErr}
	for id,key:=range keys{if err:=write(filepath.Join(root,"keys",id),[]byte(key+"\n"));err!=nil{return err}}
	for src,dest:=range map[string]string{"claude-login-ca":"certs/api","claude-login-web-ca":"certs/web"} {
		if src=="claude-login-web-ca"&&(!c.GatewayUsage||c.UsageOrganization==""){continue}
		for _,name:=range []string{"ca.pem","authority.pem"} {data,err:=os.ReadFile(filepath.Join(from,src,name));if err!=nil{return fmt.Errorf("existing certificate is missing")};if err:=write(filepath.Join(root,dest,name),data);err!=nil{return err}}
	}
	b,err=json.MarshalIndent(c,"","  ");if err!=nil{return err}
	staged:=filepath.Join(root,filepath.Base(destination))
	if err:=write(staged,append(b,'\n'));err!=nil{return err}
	if _,err=Load(staged);err!=nil{return err}
	if err=os.Remove(targetRoot);err!=nil&&!os.IsNotExist(err){return err}
	return os.Rename(root,targetRoot)
}
