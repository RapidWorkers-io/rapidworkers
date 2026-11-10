# Releasing

Every package is published from GitHub Actions by pushing a tag. Bump the version everywhere first; `python3 scripts/check_versions.py` must print only `ok` lines, and each release workflow re-runs it against the tag.

| Tag | Publishes | Workflow |
| --- | --- | --- |
| `python-v0.1.0` | PyPI: `rapidworkers`, `rapidworkers-mcp` | `release-python.yml` |
| `npm-v0.1.0` | npm: `rapidworkers`, `rapidworkers-mcp` | `release-npm.yml` |
| `rust-v0.1.0` | crates.io: `rapidworkers`, `rapidworkers-mcp` | `release-rust.yml` |
| `docker-v0.1.0` | `ghcr.io/rapidworkers-io/rapidworkers-mcp:0.1.0` and `:latest` | `release-docker.yml` |
| `go/v0.1.0` | Go module `github.com/RapidWorkers-io/rapidworkers/go` | `release-go.yml` |
| `registry-v0.1.0` | MCP Registry entry `io.rapidworkers/rapidworkers`, plus the Claude Desktop extension `rapidworkers-mcp-0.1.0.mcpb` on a GitHub release `mcpb-v0.1.0` | `release-registry.yml` |

Publish the registry entry last: the registry checks that every package listed in `server.json` exists at that version.

When the hosted server's tools change, regenerate the spec from the web repo (`node scripts/export-mcp-tools.mjs <path>/spec/tools.json`), run `python3 scripts/codegen.py`, and release new versions.

## Keeping releases anonymous

Everything below is public. Use brand accounts and a brand email, never a personal account:

- **Git commits:** this repository is configured to commit as `RapidWorkers <noreply@rapidworkers.io>` (`git config user.name` / `user.email`). Check before the first commit on a new machine.
- **GitHub:** pushes and the Actions runs they trigger show the GitHub account that pushed. Use a separate brand account (an owner of the RapidWorkers-io organization) to push and tag. Keep personal accounts' organization membership private (organization → People → your account → Private).
- **PyPI:** the project page lists maintainers by username. Use a brand username and email.
- **npm:** package metadata lists each maintainer's username and email. Use a brand username and email.
- **crates.io:** sign-in is through GitHub, and the crate page shows that GitHub login as owner. Sign in with the brand GitHub account.

## One-time setup

### PyPI (no first manual upload needed)

1. Create a PyPI account and turn on 2FA.
2. Under Account settings, then Publishing, choose **Add a new pending publisher** twice. Use owner `RapidWorkers-io`, repository `rapidworkers` and workflow `release-python.yml` for both. Set environment `pypi` for project `rapidworkers` and `pypi-mcp` for project `rapidworkers-mcp`. PyPI refuses two pending publishers with identical settings.
3. Push `python-v0.1.0`.

### npm (first version by hand)

npm lets you add a trusted publisher only to a package that already exists.

1. Create an npm account with 2FA, then run `npm login`.
2. Publish the first version by hand: `cd typescript/rapidworkers && npm publish --access public`, then the same in `typescript/mcp`.
3. On npmjs.com, open each package's Settings, then **Trusted publishing**, then GitHub Actions. Enter organization `RapidWorkers-io`, repository `rapidworkers`, workflow `release-npm.yml` and environment `npm`.
4. Later versions: push `npm-vX.Y.Z`.

### crates.io (first version by hand)

1. Sign in to crates.io with the brand GitHub account and verify an email address.
2. Create an API token with the `publish-new` scope, then run `cargo login`.
3. `cd rust && cargo publish -p rapidworkers && cargo publish -p rapidworkers-mcp`
4. On each crate's Settings page, add a trusted publisher. Enter repository `RapidWorkers-io/rapidworkers`, workflow `release-rust.yml` and environment `crates-io`. Then revoke the API token.
5. Later versions: push `rust-vX.Y.Z`.

### Docker (GitHub Container Registry)

1. Push `docker-v0.1.0`.
2. The first push creates the package as private. Open the organization's Packages tab, then `rapidworkers-mcp`, then Package settings, and change visibility to **Public**.

### Go

No account is needed. Push `go/v0.1.0`; pkg.go.dev lists the module once the proxy has fetched it.

### MCP Registry (DNS verification of rapidworkers.io)

1. Generate a key pair with OpenSSL 3. The macOS system `openssl` cannot do Ed25519, so run `brew install openssl@3` first:

   ```bash
   OPENSSL=/opt/homebrew/opt/openssl@3/bin/openssl
   $OPENSSL genpkey -algorithm Ed25519 -out mcp-registry-key.pem
   echo "v=MCPv1; k=ed25519; p=$($OPENSSL pkey -in mcp-registry-key.pem -pubout -outform DER | tail -c 32 | base64)"
   $OPENSSL pkey -in mcp-registry-key.pem -noout -text | grep -A3 "priv:" | tail -n +2 | tr -d ' :\n'; echo
   ```

2. Add the first printed line as a **TXT record on the apex** of `rapidworkers.io`. Use the name `@`, not a subdomain. Existing TXT records such as SPF can stay.
3. Add the second printed line (the private key in hex) as a repository secret named `MCP_PRIVATE_KEY`, under Settings, then Secrets and variables, then Actions. Then delete `mcp-registry-key.pem`, or store it in a password manager.
4. After the npm, PyPI, crates.io and Docker releases of the version in `server.json`, push `registry-v0.1.0` (or run the workflow by hand).

### Directories

After the registry entry is live:

- **Glama:** indexes public GitHub MCP repositories automatically and builds the Dockerfile. Claim the listing once it appears.
- **Other directories:** submit to PulseMCP, Smithery, mcp.so, and the awesome-mcp-servers list (pull request).
