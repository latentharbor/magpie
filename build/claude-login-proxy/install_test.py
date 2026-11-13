import importlib.util
import pathlib
import unittest
from unittest.mock import patch
from types import SimpleNamespace
import tempfile

spec=importlib.util.spec_from_file_location('installer',pathlib.Path(__file__).with_name('install.py'))
installer=importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)

class InstallerTests(unittest.TestCase):
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
