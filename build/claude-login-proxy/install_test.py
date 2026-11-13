import importlib.util
import pathlib
import unittest
from unittest.mock import patch
from types import SimpleNamespace
import tempfile
import json
import uuid
import io
from contextlib import ExitStack, contextmanager, redirect_stdout

spec=importlib.util.spec_from_file_location('installer',pathlib.Path(__file__).with_name('install.py'))
installer=importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)

class InstallerTests(unittest.TestCase):
    @contextmanager
    def fixture(self, folder):
        home=pathlib.Path(folder);root=home/'.config/claude-login-proxy';root.mkdir(parents=True)
        source=home/'package/claude-login-proxy';source.parent.mkdir();source.write_bytes(b'fixture binary')
        config=root/'config.json';config.write_text(json.dumps({'listen':'127.0.0.1:3426','upstreams':{},'imageUpstream':''}))
        values={'HOME':home,'ROOT':root,'CONFIG':config,'SOURCE':source,'BIN':home/'Library/Application Support/Claude Login Proxy/claude-login-proxy','PLIST':home/'Library/LaunchAgents/test-proxy.plist','MAGPIE':home/'.config/magpie'}
        def fake_call(args,required=True):
            if len(args)>1 and args[1]=='certificates':return SimpleNamespace(returncode=0,stdout='{"api":{"trusted":true}}')
            if len(args)>1 and args[1]=='status':return SimpleNamespace(returncode=0,stdout='{"running":true,"activeRequests":0}')
            return SimpleNamespace(returncode=0,stdout='')
        with ExitStack() as stack:
            for k,v in values.items():stack.enter_context(patch.object(installer,k,v))
            stack.enter_context(patch.object(installer,'call',side_effect=fake_call))
            stack.enter_context(patch.object(installer,'owner',return_value=None))
            stack.enter_context(redirect_stdout(io.StringIO()))
            yield home,root

    def test_fresh_install_creates_a_desktop_readable_profile(self):
        with tempfile.TemporaryDirectory() as folder,self.fixture(folder) as (home,root):
            installer.install()
            meta=home/'Library/Application Support/Claude-3p/configLibrary/_meta.json'
            profile_id=json.loads(meta.read_text())['appliedId']
            self.assertEqual(str(uuid.UUID(profile_id)),profile_id)
            self.assertRegex(profile_id,r'^[a-f0-9-]{36}$')
            profile=meta.parent/(profile_id+'.json')
            self.assertEqual(json.loads(profile.read_text())['egressProxyUrl'],'http://127.0.0.1:3426')
            self.assertEqual(profile.stat().st_mode&0o777,0o600)
            journal=json.loads((root/'installation.json').read_text())
            installer.restore_files(journal)
            self.assertNotIn('appliedId',json.loads(meta.read_text()))

    def test_existing_valid_profile_is_reused_without_losing_fields(self):
        with tempfile.TemporaryDirectory() as folder,self.fixture(folder) as (home,root):
            library=home/'Library/Application Support/Claude-3p/configLibrary';library.mkdir(parents=True)
            profile_id=str(uuid.uuid4());meta=library/'_meta.json'
            meta.write_text(json.dumps({'appliedId':profile_id,'selectedId':'keep'}))
            profile=library/(profile_id+'.json');profile.write_text(json.dumps({'egressProxyUrl':'http://127.0.0.1:3426','desktopHome':'standard'}))
            before=(meta.read_bytes(),profile.read_bytes())
            installer.install()
            self.assertEqual((meta.read_bytes(),profile.read_bytes()),before)

    def legacy(self, home, root):
        library=home/'Library/Application Support/Claude-3p/configLibrary';library.mkdir(parents=True)
        meta=library/'_meta.json';meta.write_text(json.dumps({'appliedId':installer.LEGACY_PROFILE_ID,'selectedId':'keep'}))
        old=library/(installer.LEGACY_PROFILE_ID+'.json');old.write_text(json.dumps({'egressProxyUrl':'http://127.0.0.1:3426','desktopHome':'standard'}))
        backup=root/'backups/original';backup.mkdir(parents=True)
        (root/'installation.json').write_text(json.dumps({'files':[],'edits':[],'backup':str(backup)}))
        return meta,old

    def test_legacy_repair_preserves_settings_and_is_reversible(self):
        with tempfile.TemporaryDirectory() as folder,self.fixture(folder) as (home,root):
            meta,old=self.legacy(home,root);old_bytes=old.read_bytes()
            self.assertTrue(installer.repair_legacy_profile())
            repaired=json.loads(meta.read_text());profile_id=repaired['appliedId']
            self.assertEqual(str(uuid.UUID(profile_id)),profile_id)
            self.assertEqual(json.loads((meta.parent/(profile_id+'.json')).read_text()),json.loads(old_bytes))
            self.assertEqual(old.read_bytes(),old_bytes)
            self.assertFalse(installer.repair_legacy_profile())
            repaired['selectedId']='changed-later';meta.write_text(json.dumps(repaired))
            installer.restore_files(json.loads((root/'installation.json').read_text()))
            restored=json.loads(meta.read_text())
            self.assertEqual(restored['appliedId'],installer.LEGACY_PROFILE_ID)
            self.assertEqual(restored['selectedId'],'changed-later')

    def test_legacy_repair_does_not_override_a_managed_pointer(self):
        with tempfile.TemporaryDirectory() as folder,self.fixture(folder) as (home,root):
            meta,_=self.legacy(home,root);data=json.loads(meta.read_text());data['hybridPointer']='https://example.invalid/policy';meta.write_text(json.dumps(data));before=meta.read_bytes()
            self.assertFalse(installer.repair_legacy_profile());self.assertEqual(meta.read_bytes(),before)

    def test_failed_profile_activation_keeps_original_metadata_and_journal(self):
        with tempfile.TemporaryDirectory() as folder,self.fixture(folder) as (home,root):
            meta,old=self.legacy(home,root);journal=root/'installation.json';before=(meta.read_bytes(),journal.read_bytes())
            original=installer.atomic
            def fail_metadata(path,data,mode=0o600):
                if path==meta:raise OSError('fixture failed metadata rename')
                return original(path,data,mode)
            with patch.object(installer,'atomic',side_effect=fail_metadata):
                with self.assertRaisesRegex(OSError,'fixture failed'):installer.repair_legacy_profile()
            self.assertEqual((meta.read_bytes(),journal.read_bytes()),before)
            self.assertEqual(sorted(p.name for p in meta.parent.glob('*.json')),sorted([meta.name,old.name]))

    def test_corrupt_legacy_profile_is_not_overwritten(self):
        with tempfile.TemporaryDirectory() as folder,self.fixture(folder) as (home,root):
            meta,old=self.legacy(home,root);old.write_text('broken');before=meta.read_bytes()
            with self.assertRaises(ValueError):installer.repair_legacy_profile()
            self.assertEqual(old.read_text(),'broken');self.assertEqual(meta.read_bytes(),before)

    def test_codex_only_owned_transport_changes(self):
        original='''model_provider = "openai"
[model_providers.old]
base_url = "http://127.0.0.1:3425"
[mcp_servers.magpie-image]
command = "/Applications/magpie.app/Contents/MacOS/magpie"
args = ["mcp", "image"]
tool_timeout_sec = 660
[mcp_servers.another]
command = "keep-me"
'''
        edits=installer.codex_image_changes(original)
        self.assertEqual(len(edits),2)
        updated=installer.replace_codex_image(original,edits)
        self.assertIn('model_provider = "openai"',updated)
        self.assertIn('tool_timeout_sec = 660',updated)
        self.assertIn('command = "keep-me"',updated)
        self.assertIn('mcp-image',updated)
        self.assertEqual(installer.replace_codex_image(updated,edits,True),original)
        changed=updated.replace('tool_timeout_sec = 660','tool_timeout_sec = 900')
        self.assertIn('tool_timeout_sec = 900',installer.replace_codex_image(changed,edits,True))

    def test_custom_transport_is_not_replaced(self):
        self.assertEqual(installer.codex_image_changes('[mcp_servers.magpie-image]\ncommand = "custom-helper"\nargs = []\n'),[])

    def test_upgrade_refuses_active_requests_without_changing_binary(self):
        with tempfile.TemporaryDirectory() as folder:
            root=pathlib.Path(folder)
            (root/'installation.json').write_text('{}')
            binary=root/'installed';binary.write_bytes(b'old')
            with patch.object(installer,'ROOT',root),patch.object(installer,'BIN',binary),patch.object(installer,'call',return_value=SimpleNamespace(returncode=0,stdout='{"running":true,"activeRequests":1}')) as call:
                with self.assertRaisesRegex(RuntimeError,'active requests'):installer.upgrade()
                self.assertEqual(binary.read_bytes(),b'old')
                self.assertEqual(call.call_count,1)

    def test_upgrade_failure_restores_binary_without_rewriting_config(self):
        with tempfile.TemporaryDirectory() as folder:
            root=pathlib.Path(folder)
            journal=root/'installation.json';journal.write_text('{"files":[]}')
            config=root/'config.json';config.write_text('keep-private-settings')
            binary=root/'installed';binary.write_bytes(b'old')
            source=root/'new';source.write_bytes(b'new')
            reads=0;boots=0
            def fake_call(args,required=True):
                nonlocal reads,boots
                if len(args)>1 and args[1]=='status':
                    reads+=1
                    return SimpleNamespace(returncode=0 if reads==1 else 1,stdout='{"running":true,"activeRequests":0}')
                if 'bootstrap' in args:
                    boots+=1
                    if required:raise RuntimeError('bootstrap fixture failure')
                return SimpleNamespace(returncode=0,stdout='')
            with patch.object(installer,'ROOT',root),patch.object(installer,'BIN',binary),patch.object(installer,'SOURCE',source),patch.object(installer,'call',side_effect=fake_call):
                with self.assertRaisesRegex(RuntimeError,'bootstrap fixture'):installer.upgrade()
            self.assertEqual(binary.read_bytes(),b'old')
            self.assertEqual(config.read_text(),'keep-private-settings')
            self.assertEqual(journal.read_text(),'{"files":[]}')
            self.assertEqual(boots,2)

if __name__=='__main__':unittest.main()
