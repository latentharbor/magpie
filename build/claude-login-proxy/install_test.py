import importlib.util
import pathlib
import unittest

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

if __name__=='__main__':unittest.main()
