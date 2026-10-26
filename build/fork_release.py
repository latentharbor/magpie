#!/usr/bin/env python3
"""Release metadata for this distribution. No machine configuration is read."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys

REPOSITORY = "latentharbor/magpie"
NOTES = ("Maintenance and compatibility improvements.\n\n"
         "macOS downloads are ad-hoc signed, not Apple notarized.\n\n"
         "<!-- lang:zh -->\n\n维护与兼容性更新。\n\n"
         "macOS 安装包使用临时代码签名，未经 Apple 公证。")
ASSETS = tuple(
    [f"magpie-darwin-{arch}.zip" for arch in ("amd64", "arm64")]
    + [f"magpie-linux-{arch}" for arch in ("amd64", "arm64")]
    + [f"magpie-windows-{arch}.exe" for arch in ("amd64", "arm64")]
    + [f"magpie-cli-{system}-{arch}" + (".exe" if system == "windows" else "")
       for system, arch in (("darwin", "amd64"), ("darwin", "arm64"),
                            ("linux", "amd64"), ("linux", "arm64"),
                            ("windows", "amd64"), ("windows", "arm64"),
                            ("android", "arm64"))]
)


def gh(path):
    return json.loads(subprocess.check_output(["gh", "api", path], text=True))


def approved(sha, head, runs):
    """A cancelled run, PR run or a green ancestor cannot approve this SHA."""
    return (bool(re.fullmatch(r"[0-9a-f]{40}", sha)) and sha == head
            and any(run.get("head_sha") == sha and run.get("head_branch") == "main"
                    and run.get("event") in ("push", "workflow_dispatch")
                    and run.get("status") == "completed" and run.get("conclusion") == "success"
                    and run.get("head_repository", {}).get("full_name") == REPOSITORY
                    for run in runs))


def approved_backend(sha, head, run, jobs, preparing=False):
    """Publication requires every backend job; preparation may overlap them."""
    if (run.get("status") not in ("in_progress", "completed")
            or run.get("conclusion") not in (None, "", "success", "failure")
            or not approved(sha, head, [dict(run, status="completed", conclusion="success")])):
        return False
    required = {
        "test (ubuntu-latest)", "test (macos-latest)", "test (windows-latest)",
        "cli (linux/amd64, nogui)", "cli (android/arm64, nogui)",
    }
    backend = [job for job in jobs if not job.get("name", "").startswith("ui (")]
    return (len(backend) == len(required)
            and {job.get("name") for job in backend} == required
            and all((job.get("status") == "completed" and job.get("conclusion") == "success")
                    or (preparing and job.get("status") == "in_progress"
                        and job.get("conclusion") in (None, ""))
                    for job in backend))


def gate(sha):
    if os.environ.get("GITHUB_REPOSITORY") != REPOSITORY:
        raise ValueError("this release belongs to a different repository")
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("a complete commit SHA is required")
    head = gh(f"repos/{REPOSITORY}/git/ref/heads/main")["object"]["sha"]
    runs = gh(f"repos/{REPOSITORY}/actions/workflows/test.yml/runs?head_sha={sha}&per_page=100")["workflow_runs"]
    if not approved(sha, head, runs):
        exception = (os.environ.get("GITHUB_EVENT_NAME") == "workflow_dispatch"
                     and os.environ.get("FORK_RELEASE_ALLOW_UI_FAILURES") == "true")
        allowed = False
        if exception:
            for run in runs:
                if not approved(sha, head, [dict(run, status="completed", conclusion="success")]):
                    continue
                result = gh(f"repos/{REPOSITORY}/actions/runs/{run['id']}/jobs?per_page=100")
                jobs = result["jobs"]
                preparing = os.environ.get("FORK_RELEASE_PREPARE") == "true"
                if result.get("total_count") == len(jobs) and approved_backend(sha, head, run, jobs, preparing):
                    allowed = True
                    print("Manual UI exception: UI acceptance is not asserted; publication requires completed backend checks."
                          if preparing else "Manual UI exception: backend checks passed; UI acceptance is not asserted.", file=sys.stderr)
                    break
        if not allowed:
            raise ValueError("release requires successful Test on exact current main, or an explicit manual UI exception with all backend checks passed")
    return sha


def next_version(tags):
    versions = [tuple(map(int, m.groups())) for tag in tags
                if (m := re.fullmatch(r"v(\d+)\.(\d+)\.(\d+)", tag))]
    major, minor, patch = max(versions, default=(0, 1, 1330))
    return f"{major}.{minor}.{patch + 1}"


def release_plan(sha, releases, tags):
    same = [r for r in releases if r.get("target_commitish") == sha
            and re.fullmatch(r"v\d+\.\d+\.\d+", r.get("tag_name", ""))]
    if any(not r.get("draft") for r in same):
        return None
    draft = next((r["tag_name"][1:] for r in same if r.get("draft")), None)
    return draft or next_version(tags)


def checked_plan(sha, releases, tags, expected=None):
    version = release_plan(sha, releases, tags)
    if expected is not None and version != expected:
        raise ValueError("release plan changed during the build; refusing publication")
    return version


def key_bytes(text, length):
    try:
        raw = base64.b64decode(text, validate=True)
    except (ValueError, TypeError):
        raise ValueError("update signing key must be canonical base64") from None
    if len(raw) != length or base64.b64encode(raw).decode() != text:
        raise ValueError("update signing key has the wrong size or encoding")
    return raw


def public_key():
    key = Path(__file__).with_name("fork-update-public-key.txt").read_text().strip()
    key_bytes(key, 32)
    return key


def private_key(public):
    seed = key_bytes(os.environ.get("FORK_RELEASE_PRIVATE_KEY", ""), 32)
    # RFC 8410 PKCS#8 encoding of an Ed25519 seed. The private key is passed
    # on stdin, never as an argument, log, repository file or uploaded asset.
    private_der = bytes.fromhex("302e020100300506032b657004220420") + seed
    result = subprocess.run(["openssl", "pkey", "-inform", "DER", "-pubout", "-outform", "DER"],
                            input=private_der, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if result.returncode or result.stdout != bytes.fromhex("302a300506032b6570032100") + key_bytes(public, 32):
        raise ValueError("release signing seed does not match the pinned public key")
    return private_der


def sign_manifest(folder, public):
    private_der = private_key(public)
    result = subprocess.run(["openssl", "pkeyutl", "-sign", "-rawin", "-inkey", "/dev/stdin",
                             "-keyform", "DER", "-in", str(folder / "latest.json")],
                            input=private_der, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if result.returncode or len(result.stdout) != 64:
        raise ValueError("Ed25519 manifest signing failed")
    (folder / "latest.json.sig").write_text(base64.b64encode(result.stdout).decode() + "\n")


def platform(name):
    parts = name.removesuffix(".zip").removesuffix(".exe").split("-")
    return "/".join(parts[-2:])


def manifest(folder, version, history):
    if not re.fullmatch(r"\d+\.\d+\.\d+", version):
        raise ValueError("bad release version")
    present = {p.name for p in folder.iterdir()}
    if present != set(ASSETS) or any(not (folder / name).is_file() or (folder / name).is_symlink() for name in ASSETS):
        raise ValueError("release directory must contain exactly the expected binaries and app archives")
    home = f"https://github.com/{REPOSITORY}/releases"
    assets = {}
    for name in ASSETS:
        path = folder / name
        if path.stat().st_size == 0:
            raise ValueError(f"empty release asset: {name}")
        with path.open("rb") as binary:
            sha = hashlib.sha256()
            for chunk in iter(lambda: binary.read(1024 * 1024), b""):
                sha.update(chunk)
            digest = sha.hexdigest()
        assets[name] = {"platform": platform(name), "url": f"{home}/download/v{version}/{name}",
                        "size": path.stat().st_size, "sha256": digest}
    release = {"schema": 1, "repository": REPOSITORY,
               "version": version, "notes": NOTES, "url": f"{home}/tag/v{version}", "assets": assets}
    notes = [{"version": version, "notes": NOTES, "url": release["url"]}]
    for old in history:
        tag = old.get("tag_name", "")
        if (not old.get("draft") and not old.get("prerelease")
                and re.fullmatch(r"v\d+\.\d+\.\d+", tag) and tag != "v" + version):
            # Only generic distribution notes are published, never a diff or commit log.
            notes.append({"version": tag[1:], "notes": NOTES, "url": f"{home}/tag/{tag}"})
    (folder / "latest.json").write_text(json.dumps(release, indent=2) + "\n")
    (folder / "notes.json").write_text(json.dumps({"releases": notes[:40]}, indent=2) + "\n")
    (folder / "SHA256SUMS").write_text("".join(f"{assets[name]['sha256']}  {name}\n" for name in sorted(assets)))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    check = sub.add_parser("gate")
    check.add_argument("sha")
    check.add_argument("--version", help="require the initial build's unchanged release plan")
    make = sub.add_parser("manifest")
    make.add_argument("folder", type=Path)
    make.add_argument("version")
    make.add_argument("history", type=Path)
    sign = sub.add_parser("sign")
    sign.add_argument("folder", type=Path)
    sub.add_parser("check-key")
    flags = sub.add_parser("flags")
    flags.add_argument("version")
    args = parser.parse_args()
    if args.command == "gate":
        gate(args.sha)
        tags = gh(f"repos/{REPOSITORY}/tags?per_page=100")
        releases = gh(f"repos/{REPOSITORY}/releases?per_page=100")
        version = checked_plan(args.sha, releases, [tag["name"] for tag in tags], args.version)
        # Releases remain serialized; no tag is created until every signed asset exists.
        print(f"eligible={'true' if version else 'false'}\nsha={args.sha}\nversion={version or ''}")
    elif args.command == "manifest":
        manifest(args.folder, args.version, json.loads(args.history.read_text()))
    elif args.command == "sign":
        sign_manifest(args.folder, public_key())
    elif args.command == "check-key":
        private_key(public_key())
    else:
        if not re.fullmatch(r"\d+\.\d+\.\d+", args.version):
            raise ValueError("bad build version")
        prefix = "github.com/yetone/magpie/internal/update"
        print(f"-s -w -X main.version={args.version} -X {prefix}.distributionTrust={REPOSITORY} "
              f"-X {prefix}.distributionPublicKey={public_key()} -X {prefix}.distributionVersion={args.version}")


if __name__ == "__main__":
    main()
