# One Magpie gateway for several devices

This Compose file runs the [latentharbor Magpie distribution](https://github.com/latentharbor/magpie) on one server or NAS. It keeps providers, Claude Team configuration, sign-ins, gateway keys, plugins and usage in the named `magpie-config` volume. Recreating the container with the same volume keeps that state. Run **one** controller against a Team configuration; do not start a second container on the same volume.

## Start

From this directory:

```sh
cp .env.example .env
openssl rand -hex 32
```

Put that random value in `.env` as `MAGPIE_WEB_KEY`. The example binds both ports to the host's loopback address. Then run:

```sh
docker compose up -d
docker compose ps
```

Open `http://127.0.0.1:3430/?k=<MAGPIE_WEB_KEY>` on the server. From another computer, forward the admin port over SSH first: `ssh -L 3430:127.0.0.1:3430 user@server`, then open the same local URL. Keep the admin port private; its key grants access to providers and credentials.

For each client device, make a separate gateway key:

```sh
docker compose exec magpie magpie gateway-key add "Laptop"
docker compose exec magpie magpie gateway-key add "Desktop"
```

To reach the gateway from another device, set `MAGPIE_BIND_IP` to the server's VPN or LAN IP and `MAGPIE_PUBLIC_URL` to the matching reachable URL, then run `docker compose up -d` again. Prefer a VPN such as Tailscale/WireGuard or HTTPS reverse proxy for remote use; a plain HTTP key is visible to the network carrying it. Do not expose the admin port 3430. Each client supplies its own gateway key, even if it runs on the Docker host. In a local Magpie app, add the server as **Remote magpie**; an API client can use `http://<server>:3425/v1` for OpenAI-compatible calls or the base URL for Anthropic calls.

Claude Team and other subscription accounts are configured once in the server's browser UI. A Claude OAuth sign-in may end at a browser `localhost` callback on your laptop; paste that complete callback URL into the server UI's sign-in form. If a subscription needs the official Claude Code binary, install it inside the container with `docker compose exec magpie magpie claude-code install`. The downloaded binary and sign-in files stay in `/config`.

When moving an existing Team organization to the server, disable that organization's local Team controller before enabling its server copy. Each organization needs one controller across all devices: the local process lock cannot coordinate separate computers. Client Magpie installations should use **Remote magpie** for that organization instead of importing a second active Team pool.

**Claude Desktop official-login mode is local to each Desktop.** Its HTTPS proxy, local certificate trust and native profile cannot be provided by the server container alone. Keep a local Magpie on each such Mac and add this server as Remote magpie with that device's key. In **Keep Claude signed in**, map the models to the remote provider and enable **Let Remote magpie manage inference** (`magpie claude-login remote on`). This direct path preserves native Threads and cache touches; the NAS owns routing, limits and request history. Quota-display handling stays local and does not replace official subscription permissions. Private CA keys never move to the NAS. The central container holds the inference accounts and seat controller. Direct third-party API clients can call the container without a local Magpie.

Update with `docker compose pull && docker compose up -d`. The named volume remains attached. Back up the volume before moving to a new server; never share it or the `.env` file because both hold credentials. Check `docker compose ps` for the image's health status after an update.
