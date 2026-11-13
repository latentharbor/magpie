"""Install a per-user client using existing trust, with a reversible handoff."""
from pathlib import Path
import datetime
import json
import os
import plistlib
import shutil
import signal
import subprocess
import sys
import time
import urllib.request

HOME = Path.home()
SOURCE = Path(__file__).resolve().parent / 'claude-login-proxy'
ROOT = HOME / '.config/claude-login-proxy'
CONFIG = ROOT / 'config.json'
BIN = HOME / 'Library/Application Support/Claude Login Proxy/claude-login-proxy'
LABEL = 'io.latentharbor.claude-login-proxy'
PLIST = HOME / 'Library/LaunchAgents' / (LABEL + '.plist')
DOMAIN = 'gui/' + str(os.getuid())
MAGPIE = HOME / '.config/magpie'
APP = Path('/Applications/magpie.app')

def call(args, required=True):
    p = subprocess.run([str(x) for x in args], capture_output=True, text=True)
    if required and p.returncode:
        raise RuntimeError(Path(str(args[0])).name + ' failed: ' + p.stderr.strip())
    return p

def atomic(path, data, mode=0o600):
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + '.proxy-new')
    with temporary.open('wb') as f:
        f.write(data); f.flush(); os.fsync(f.fileno())
    temporary.chmod(mode); temporary.replace(path)

def owner():
    p = call(['/usr/sbin/lsof', '-nP', '-t', '-iTCP:3425', '-sTCP:LISTEN'], False)
    if not p.stdout.strip(): return None
    ids = set(p.stdout.split())
    if len(ids) != 1: raise RuntimeError('Several processes own the local gateway port')
    pid = int(next(iter(ids)))
    command = call(['/bin/ps', '-p', pid, '-o', 'comm=']).stdout.strip()
    if command != str(APP / 'Contents/MacOS/magpie'):
        raise RuntimeError('Port 3425 belongs to another service')
    return pid

def alive(pid):
    try: os.kill(pid, 0); return True
    except ProcessLookupError: return False

def wait_idle(pid):
    if not pid: return
    until = time.monotonic() + 1800
    stable = 0
    while time.monotonic() < until:
        with urllib.request.urlopen('http://127.0.0.1:3425/v1/magpie/concurrency', timeout=5) as r: d = json.load(r)
        counts = [d.get(k) for k in ('requests','tools','waiting')]
        if d.get('object') != 'concurrency' or any(type(n) is not int for n in counts):
            raise RuntimeError('Cannot verify gateway activity')
        stable = stable + 1 if not any(counts) else 0
        if stable >= 3: return
        time.sleep(1)
    raise RuntimeError('Gateway is busy; installation did not interrupt it')

def restore():
    journal = json.loads((ROOT / 'installation.json').read_text())
    if BIN.exists():
        status = call([BIN,'status'], False)
        if status.returncode == 0 and json.loads(status.stdout).get('activeRequests', 1):
            raise RuntimeError('Wait for active proxy requests before restoring')
    call(['/bin/launchctl','bootout',DOMAIN+'/'+LABEL], False)
    restore_files(journal)
    call(['/usr/bin/open','-a',APP])
    print('Standalone service stopped; previous Magpie configuration restored.')

def restore_files(journal):
    edited = {x['path'] for x in journal.get('edits',[])}
    for item in reversed(journal.get('edits',[])):
        p=Path(item['path'])
        if not p.exists():continue
        d=json.loads(p.read_text());parent=d
        for k in item['keys'][:-1]:
            parent=parent.get(k,{})
        key=item['keys'][-1]
        if parent.get(key)!=item['after']:continue
        if item['had']:parent[key]=item['before']
        else:parent.pop(key,None)
        atomic(p,json.dumps(d,indent=2).encode())
    for item in reversed(journal['files']):
        if item['path'] in edited:continue
        p = Path(item['path'])
        if item['copy']:
            atomic(p,Path(item['copy']).read_bytes(),item['mode'])
        elif p.exists(): p.unlink()

def install():
    os.umask(0o077)
    call(['/usr/bin/codesign','--verify','--strict',SOURCE])
    if not CONFIG.exists():
        if not (MAGPIE/'claude-login.json').exists():
            raise RuntimeError('Run init first; see README.md for a fresh computer')
        call([SOURCE,'import-magpie'])
    certs = json.loads(call([SOURCE,'certificates']).stdout)
    if any(not c.get('Trusted', c.get('trusted',False)) for c in certs.values()):
        raise RuntimeError('Grant SSL trust to the public CA files listed by the certificates command first')
    cfg = json.loads(CONFIG.read_text())
    image = cfg['upstreams'][cfg['imageUpstream']]
    keypath = Path(image['keyFile'])
    if not keypath.is_absolute(): keypath = ROOT/keypath
    key = keypath.read_text().strip()
    req = urllib.request.Request(image['url'].rstrip('/')+'/api/hello',headers={'Authorization':'Bearer '+key})
    with urllib.request.urlopen(req,timeout=10) as r: hello=json.load(r)
    if hello.get('name') != 'magpie': raise RuntimeError('NAS gateway did not identify itself')
    if (ROOT/'installation.json').exists():
        raise RuntimeError('Already installed; keep the existing configuration when updating the binary')
    base=HOME/'Library/Application Support'
    meta=base/'Claude-3p/configLibrary/_meta.json'
    d=json.loads(meta.read_text()) if meta.exists() else {}
    applied=d.get('appliedId','')
    active=meta.parent/(applied+'.json') if applied and '/' not in applied and '\\' not in applied else None
    existing_proxy=json.loads(active.read_text()).get('egressProxyUrl') if active and active.exists() else None
    reuse_profile=existing_proxy=='http://'+cfg['listen']
    if not reuse_profile and 'hybridPointer' in d:raise RuntimeError('Desktop has a managed profile; configure its proxy through that policy')
    for p in [base/'Claude/claude_desktop_config.json',HOME/'.claude.json',base/'Claude/config.json',base/'Claude-3p/config.json']:
        if p.exists():json.loads(p.read_text())
    pid = owner(); wait_idle(pid)
    backup = ROOT/'backups'/datetime.datetime.now().strftime('%Y%m%d-%H%M%S')
    backup.mkdir(parents=True,mode=0o700)
    journal = {'files':[], 'edits':[], 'backup':str(backup)}
    def remember(p):
        if any(x['path']==str(p) for x in journal['files']):return
        copy = backup/str(len(journal['files']))
        mode = 0o600
        if p.exists():
            mode=p.stat().st_mode&0o777;shutil.copy2(p,copy)
            saved=str(copy)
        else: saved=None
        journal['files'].append({'path':str(p),'copy':saved,'mode':mode})
        atomic(ROOT/'installation.json',json.dumps(journal,indent=2).encode())
    def change(p,keys,value):
        remember(p);d=json.loads(p.read_text()) if p.exists() else {};parent=d
        for k in keys[:-1]:parent=parent.setdefault(k,{})
        key=keys[-1]
        journal['edits'].append({'path':str(p),'keys':keys,'had':key in parent,'before':parent.get(key),'after':value})
        atomic(ROOT/'installation.json',json.dumps(journal,indent=2).encode())
        parent[key]=value;atomic(p,json.dumps(d,indent=2).encode())
    stopped = False
    try:
        if pid:
            if owner()!=pid:raise RuntimeError('Gateway owner changed')
            os.kill(pid,signal.SIGTERM)
            for _ in range(60):
                if not alive(pid):break
                time.sleep(0.5)
            else:raise RuntimeError('Magpie did not stop gracefully')
            stopped=True
        BIN.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
        remember(BIN);atomic(BIN,SOURCE.read_bytes(),0o700)
        license_file=SOURCE.parent/'LICENSE'
        if license_file.exists():
            installed_license=BIN.parent/'LICENSE';remember(installed_license);atomic(installed_license,license_file.read_bytes())
        old_start=HOME/'Library/LaunchAgents/com.yetone.magpie.plist'
        remember(old_start)
        if old_start.exists():old_start.unlink()
        call(['/bin/launchctl','bootout',DOMAIN+'/com.yetone.magpie'],False)
        p=MAGPIE/'claude-login.json'
        if p.exists():
            change(p,['enabled'],False)
        # The endpoint and trusted CAs are unchanged; existing Desktop login
        # profiles keep working. A fresh installation gets its own 1p profile.
        profile=base/'Claude-3p/configLibrary'/('claude-login-proxy.json')
        changes=[] if reuse_profile else [(profile,'egressProxyUrl','http://'+cfg['listen']),(meta,'appliedId','claude-login-proxy')]
        changes += [(base/'Claude/config.json','deploymentMode','1p'),(base/'Claude-3p/config.json','deploymentMode','1p')]
        for p,k,v in changes:
            change(p,[k],v)
        for p in [base/'Claude/claude_desktop_config.json',HOME/'.claude.json']:
            change(p,['mcpServers','magpie-image'],{'command':str(BIN),'args':['mcp-image','--config',str(CONFIG)]})
        logs=HOME/'Library/Logs/Claude Login Proxy';logs.mkdir(parents=True,exist_ok=True,mode=0o700)
        agent={'Label':LABEL,'ProgramArguments':[str(BIN),'serve','--config',str(CONFIG)],'RunAtLoad':True,'KeepAlive':{'SuccessfulExit':False},'ThrottleInterval':10,'ExitTimeOut':60,'LimitLoadToSessionType':'Aqua','ProcessType':'Background','Umask':63,'StandardOutPath':str(logs/'service.log'),'StandardErrorPath':str(logs/'error.log')}
        remember(PLIST);atomic(PLIST,plistlib.dumps(agent))
        call(['/bin/launchctl','bootstrap',DOMAIN,PLIST])
        for _ in range(40):
            status=call([BIN,'status'],False)
            if status.returncode==0 and json.loads(status.stdout).get('running'):
                print(status.stdout.strip());print('Installed. Magpie GUI is no longer required.');return
            time.sleep(1)
        raise RuntimeError('Standalone service did not become ready')
    except Exception:
        call(['/bin/launchctl','bootout',DOMAIN+'/'+LABEL],False)
        restore_files(journal)
        (ROOT/'installation.json').rename(backup/'failed-installation.json')
        if stopped:call(['/usr/bin/open','-a',APP],False)
        raise

if '--restore' in sys.argv:restore()
else:install()
