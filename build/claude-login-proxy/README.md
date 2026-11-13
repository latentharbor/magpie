# Claude Login Proxy

A small macOS background client for a remote Magpie gateway. It keeps Claude
Desktop's official login, routes selected inference to the NAS, handles local
Code usage display, and forwards the image MCP tool. It has no GUI, subscription
accounts, seat controller or local inference engine.

The NAS remains responsible for real usage, subscriptions, routing and image
generation. Official subscription permissions and account verification are not
changed. Each computer should have a separate NAS gateway key.

## Existing Magpie setup

Run `Install.command`. It imports only the remote gateway key, exact model
mappings and existing restricted certificates into
`~/.config/claude-login-proxy`. It does not copy subscription credentials or add
certificate trust. The installer waits for idle, backs up the changed files,
stops the Magpie GUI gateway, and starts this client's user LaunchAgent.

Claude's `magpie-image` entry is redirected to this binary's `mcp-image` command.
The image is generated on NAS; local reference images are uploaded only when
explicitly named in a tool call. Generated images are saved into the project
when the client supplies roots, otherwise under `~/Pictures/Claude Proxy`.
Existing destination files are never overwritten. A small image-only relay
keeps old MCP subprocesses usable until Claude reconnects them; it is not a
replacement for a general local inference gateway.

## Fresh computer

Start Claude Desktop and sign in first. Create a device key on NAS, put it in a
private file, then prepare the client (choose `--provider` when NAS has several
Claude providers):

```sh
./claude-login-proxy init --gateway http://NAS:3425 --key-file /private/device-key --organization YOUR-DESKTOP-ORGANIZATION-UUID
./claude-login-proxy certificates
```

The public CA files are `certs/api/ca.pem` and `certs/web/ca.pem` under the
configuration directory. Inspect their fingerprints and explicitly grant SSL
trust in the login Keychain before installing. Import only those public files;
`authority.pem` contains a private key and stays on this computer. The installer
does not change certificate trust or disable TLS validation. Then run
`Install.command` and reopen Claude Desktop to load its proxy configuration.

## Runtime

The HTTPS proxy listens on `127.0.0.1:3426`; the image compatibility relay uses
`127.0.0.1:3425`. The user LaunchAgent is
`io.latentharbor.claude-login-proxy`. It starts after login and restarts after a
crash. Logs are in `~/Library/Logs/Claude Login Proxy`, with no request bodies,
gateway keys or official credentials logged.

```sh
~/Library/Application\ Support/Claude\ Login\ Proxy/claude-login-proxy status
```

Only the native API and explicitly configured organization usage host are
decrypted. Selected failures never fall back to the official paid endpoint.
Leaf certificates renew using the same trusted, domain-restricted CA.
Configuration changes to model mappings or the NAS address apply to subsequent
requests. Listener or certificate-directory changes require a service restart.

Use `Restore Magpie.command` to stop the independent service and restore the
configuration files saved at installation. Finish active work before restoring.
The old Magpie app remains installed for that purpose.
