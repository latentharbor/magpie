# Distribution builds and updates

This distribution retains the application name `magpie`, bundle identifier
`com.yetone.magpie`, file locations and user settings. Releases and the update
feed belong to `latentharbor/magpie`, not upstream's release repository.

## Trust boundary

The release workflow publishes **ad-hoc signed, unnotarized macOS apps**.
An independent Ed25519 signature authenticates the distribution; it is not an
Apple Developer ID certificate and does not grant Gatekeeper approval. The
workflow and updater do not remove quarantine attributes, disable Gatekeeper
or claim Apple notarization. The first trusted distribution build must be
installed manually. An existing Apple-signed application cannot silently
switch to this trust model through its updater.

[`distribution.go`](../internal/update/distribution.go) verifies the detached
signature on the exact `latest.json` bytes before interpreting the manifest.
The trusted raw 32-byte public key is compiled into opted-in builds, together
with the repository identity and installed version. Neither a release, an
environment variable nor a settings file can replace it. A missing or invalid
key fails closed.

The manifest requires schema 1, this repository, a stable release version and
the complete supported platform asset set. Each asset binds its platform,
exact repository/version download URL, positive bounded size and SHA-256.
Duplicate JSON keys, case aliases, unknown fields, trailing JSON, mismatched
platforms and noncanonical key/signature encodings are rejected. Staging also
checks that the authenticated object has not changed and that its version is
strictly newer than the version compiled into the running build. A replay
cannot install an equal or older version. It is still possible to withhold
updates or replay an authentic release newer than the installed one; this
scheme does not provide an independent transparency log or freshness oracle.

Downloads are bounded by the signed size and checked against the signed hash.
HTTPS redirects may remain within this repository or reach GitHub's asset
CDNs, but cannot cross to another repository or arbitrary host. Signed
distribution requests do not use arbitrary download mirrors; the configured
transport proxy still applies. Corrupt or incomplete downloads are removed.
Automatic app installation additionally requires a valid strict local code
signature on the downloaded bundle. Only an explicitly opted-in build with an
authenticated manifest may use the ad-hoc-to-ad-hoc path. The existing Apple
same-TeamIdentifier path remains; a failed signer read is not proof of an
ad-hoc signature. The ad-hoc path strictly verifies both bundles.

Signed app archives are validated and extracted by the same ZIP reader, not
passed to a second extractor. Only the stock ASCII `magpie.app` tree with
regular files/directories is accepted; traversal, case aliases, symlinks,
special entries, missing executables and excessive expanded size are rejected
before writing files. Standard ditto AppleDouble metadata is accepted but not
restored. The bundle identifier (`com.yetone.magpie`), executable (`magpie`),
package type and both plist version fields must match the authenticated
release before either signing path can accept it.

The updater displays notes from the authenticated latest manifest. The generic
historical `notes.json` is a compatibility artifact, not an installation trust
source. An opted-in app currently shows the latest applicable note rather
than retrieving each historical signed manifest.

## Key setup

The maintainer creates and securely backs up an Ed25519 key outside this
repository. Do not paste the private key into chat or commit it.

- Commit `build/fork-update-public-key.txt`: canonical standard base64 of the
  raw **32-byte public key**, with an optional final newline.
- Set repository Actions secret `FORK_RELEASE_PRIVATE_KEY`: canonical standard
  base64 of the raw **32-byte private seed**, not a 64-byte expanded private
  key, PEM text or DER file. The secret value has no whitespace.

`python3 build/fork_release.py check-key` verifies the secret's derived public
key matches the checked-in key. The script uses OpenSSL Ed25519 with the
private key on stdin; it never writes it to the repository, command arguments
or release assets. OpenSSL 3 or newer is required on the publishing runner.
Unit tests use a publicly documented RFC 8032 fixture, not a release key.

`python3 build/fork_release.py flags VERSION` emits the linker flags for
`main.version` and these internal updater values:

- `distributionTrust=latentharbor/magpie`
- `distributionPublicKey=<checked-in canonical base64 public key>`
- `distributionVersion=<stable VERSION>`

The build workflows pass those flags to every platform. A plain development
build has no implicit Ed25519 trust. Existing signed installations need a
manual, independently verified bootstrap before using a different signing
identity or key. Key rotation is deliberately manual and fail-closed: changing
the publication key alone makes old clients refuse future manifests. The
current implementation has no automatic cross-signing or emergency key
replacement mechanism. A leaked release seed can authorize malicious updates;
rotate it and install a newly reviewed trusted build manually.

## Publication and synchronization

[`fork-release.yml`](../.github/workflows/fork-release.yml) runs after successful
`Test` on `main`, or by explicit workflow dispatch. Its gate verifies a completed
successful Test run on the **exact current main SHA**, not a pull request,
ancestor, cancelled run or another repository. A main change during the build
stops publication. Release jobs are serialized; a SHA already published is
skipped, and a matching incomplete draft can be retried.
Every publication recheck binds the original build version as well as its
SHA. A changed tag/release plan or an already-published SHA aborts publication,
instead of making an older build Latest after a concurrent manual release.

The workflow creates 13 platform binaries/archives, `SHA256SUMS`, `latest.json`,
`latest.json.sig` and generic `notes.json`: 17 assets. The metadata generator
rejects extra files. Only the explicit artifact paths are uploaded, never
account data, local validation logs or machine configurations. A complete
draft becomes Latest only after all builds, signature generation and final
main/CI checks succeed. Notes describe maintenance generically and are not
generated from commit messages or private configuration.

[`fork-sync.yml`](../.github/workflows/fork-sync.yml) checks upstream every six
hours. It performs a normal merge, preserving distribution changes or failing
without updating main on conflict. A concurrent push is rejected; there is no
force push or automatic preference that discards one side. It explicitly
dispatches Test after a merge, because a GITHUB_TOKEN push does not itself
trigger push workflows. That Test run can then start the guarded release.
Workflow permissions, branch protection, Actions availability and signing-key
configuration still need repository-owner setup. A failed merge or build
requires maintainer attention; it is never silently published.

## Verification

Targeted local checks:

```sh
python3 -m unittest discover -s build -p 'fork*_test.py'
go test -race ./internal/update -run '^Test(Distribution|Fork)' -count=20
go test -race ./internal/update -count=1
```

These tests use local fixtures and temporary files. They cover strict signed
manifest validation, key mismatch, raw-byte tampering, version rollback,
authenticated-object mutation, repository redirects, byte/size tampering,
archive paths/symlinks/expanded size, app identity/version, ad-hoc opt-in and
Apple downgrade rejection. On macOS a local temporary Mach-O app is ad-hoc
signed, archived with ditto, extracted and strictly signature-verified; this
also checks that AppleDouble sidecars do not invalidate the app seal. The
release-plan test rejects a version change during publication. These checks
are not a claim that GitHub publication or Gatekeeper behavior has been tested
with real release assets.
