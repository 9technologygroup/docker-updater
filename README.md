# dup

**Docker updater.** Pulls and restarts a named Docker Compose stack, verifies it came up healthy, and rolls back automatically if it did not.

Trigger it from a webhook (n8n, GitHub releases, anything that can POST), from the command line, or let it poll and update itself on a schedule with a soak delay.

```
$ dup list
dup 1.0.0  4 stacks configured, 2 on auto update
api 127.0.0.1:7788   inbound token, github   outbound https://n8n.example.com/webhook/dup

STACK  AUTO  EVERY  SOAK  ROLLBACK  SERVICES  DIR
app    yes   6h     30m   yes       all       /opt/app
api    yes   12h    2h    yes       api       /opt/api
db     no    -      -     yes       all       /opt/db
proxy  no    -      -     no        all       /opt/proxy

Not covered by dup
  compose project  monitoring  /opt/monitoring        running(3)
  loose container  watchtower  containrrr/watchtower  running
```

The caller names a **stack**, never a path and never a command. Everything else comes from a root-owned config file.

Requires Linux, systemd and Docker Compose v2.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/9technologygroup/docker-updater/main/install.sh | sudo sh
```

That downloads the release for your architecture, verifies it against `checksums.txt`, creates the `dup` system account, installs both binaries and both units, generates a bearer token and a GitHub webhook secret, and sets the file ownership the security model depends on.

Or with a package:

```sh
# Debian / Ubuntu
curl -fsSLO https://github.com/9technologygroup/docker-updater/releases/latest/download/dup_linux_amd64.deb
sudo dpkg -i dup_linux_amd64.deb

# RHEL / Fedora / Rocky
sudo rpm -i https://github.com/9technologygroup/docker-updater/releases/latest/download/dup_linux_amd64.rpm

# Alpine
sudo apk add --allow-untrusted dup_linux_amd64.apk
```

Or from source:

```sh
git clone https://github.com/9technologygroup/docker-updater
cd docker-updater
make build-linux
sudo ./install.sh
```

Both binaries go to `/usr/bin`, the same place the packages put them.

The installer deliberately does **not** create `/etc/dup/config.yml` or start anything. It drops a reference config next to it and tells you what to do. Point it at your stacks, in this order:

```sh
sudo cp /etc/dup/config.example.yml /etc/dup/config.yml
sudo chown root:dup /etc/dup/config.yml && sudo chmod 0640 /etc/dup/config.yml
sudo nano /etc/dup/config.yml   # or vim, or whatever you use

sudo dup cert      # only if you set tls.self_signed, see Exposing the port
sudo dup check     # the config parses and matches this host
sudo dup audit     # the dup account cannot rewrite what runs as root
sudo systemctl enable --now dup-agent dup
```

Then check on it:

```sh
systemctl status dup-agent dup
journalctl -u dup -u dup-agent -f
sudo dup list
sudo dup status
```

`/etc/dup/config.yml` is `root:dup 0640`, so these read it as root. To run them as
yourself, join the group once: `sudo usermod -aG dup $USER`, then log out and back in.

### Upgrading

`dup version` tells you when there is a newer release:

```
$ dup version
dup v1.3.0 (4b81de7)
a newer dup is available: v1.4.0 (you have v1.3.0)
  upgrade:  curl -fsSL https://raw.githubusercontent.com/9technologygroup/docker-updater/main/install.sh | sudo sh
  notes:    https://github.com/9technologygroup/docker-updater/releases/tag/v1.4.0
```

The version itself goes to stdout and the advisory to stderr, so `dup version` stays safe to capture in a script. The running service checks once at startup and daily after that, logging `a newer dup is available`, and `GET /v1/version` reports the same thing from cache. Set `DUP_NO_UPDATE_CHECK=1` to switch all of it off, `dup version --check` to check right now, and `DUP_GITHUB_TOKEN` if a shared egress address runs into the unauthenticated rate limit.

To upgrade, re-run the installer or `apt install --only-upgrade dup`. Either replaces both binaries and leaves `/etc/dup/config.yml`, the bearer token and the GitHub secret alone.

```sh
curl -fsSL https://raw.githubusercontent.com/9technologygroup/docker-updater/main/install.sh | sudo sh
```

The installer validates your existing config **with the new binary before replacing anything**, so a config the new version rejects leaves the old one running rather than half-applying. Pin a specific version with `DUP_VERSION=v1.2.3` in front of it.

There is deliberately no `dup upgrade`. The binaries are `root:root` and the service runs unprivileged, so self-replacement would either need root in the agent or break the privilege split, and a tool built on health checks and rollback should not swap itself out with a mechanism that has neither.

## Commands

| Command | What it does |
|---|---|
| `dup list` | Stacks, their update policy, and every compose project or container on the host that dup is **not** covering |
| `dup status [stack]` | Recent update jobs, newest first |
| `dup update <stack>` | Trigger an update. `--tag`, `--dry-run`, `--force`, `--reason`, `--wait`. `--dry-run` pulls and reports what would change without recreating anything |
| `dup check` | Validate the config |
| `dup audit` | Verify the service account cannot rewrite what runs as root |
| `dup cert` | Generate the self-signed TLS certificate (run as root) |
| `dup version` | Version and commit, and whether a newer release is out. `--full` adds build date, toolchain, licence, source and the latest release. `--check` forces a check, `--no-check` skips it |
| `dup serve` | The unprivileged HTTP API (systemd runs this) |
| `dup-agent` | The privileged agent (systemd runs this) |

Flags and the stack name work in either order, so `dup update --dry-run web` and `dup update web --dry-run` do the same thing.

`dup list` is the one to reach for first. It answers "what is dup responsible for, what is waiting to apply, and what is quietly drifting outside it".

## Exposing the port

Three options. dup does not care which you pick, but it will not serve plaintext off-loopback by accident.

| Option | When | Config |
|---|---|---|
| **Loopback + reverse proxy** | You already run NPM, Caddy or Traefik. Preferred if the proxy is not itself managed by dup. | `listen: "127.0.0.1:7788"`, nothing else |
| **dup terminates TLS itself** | dup manages the proxy's own stack, or you do not want a proxy in the path at all | `tls.self_signed: true`, or point `tls.cert_file`/`key_file` at a wildcard you already have |
| **Plaintext on the network** | Never, unless something else is doing the encrypting | `allow_non_loopback: true`, and dup will complain in the log |

### Why you may not want the proxy

If dup manages the reverse proxy's own compose stack, `dup update proxy` restarts the thing serving dup. The update itself survives, because the agent owns its lifetime rather than the caller's connection, but the caller's request dies mid-flight and you cannot reach dup to see what happened until the proxy is back. Terminating TLS in dup removes that circular dependency.

```bash
dup cert            # generates and persists a self-signed cert, prints the SHA-256 fingerprint
dup cert --force    # replaces it
```

`dup cert` must be run as root, and refuses before writing anything if it is not, so the key always lands `root:dup 0640` where the service can read it. It will not overwrite an existing certificate unless you pass `--force`, and it will not touch a certificate you manage yourself (`self_signed: false`). The installer runs it for you on upgrade when `self_signed: true`.

The generated certificate is a plain server certificate, not a CA, so importing it cannot be used to vouch for any other name. `dup update` and `dup status` trust it automatically by reading `cert_file`, with verification left on.

Self-signed means other clients will not trust it until told to. Pin the printed fingerprint, or import the cert, rather than disabling verification globally in n8n.

There is deliberately no ACME support. It would need port 80 reachable or DNS API credentials on every host, and that is a larger blast radius than the problem justifies.

## Restricting who can reach it

```yaml
allow_from: ["10.0.0.0/8", "192.168.1.5"]
trusted_proxies: ["127.0.0.1", "172.18.0.0/16"]
```

`allow_from` is an allowlist of IPs and CIDRs. Empty means anything that can reach the port, which is the right default when you are already bound to loopback.

`trusted_proxies` is the part people get wrong. Behind a reverse proxy every request arrives from `127.0.0.1`, so an allowlist would either block everything or be useless. dup only believes `X-Forwarded-For` when the direct peer is itself in `trusted_proxies`, and then takes the rightmost hop that is not a known proxy. From anywhere else the header is ignored entirely, so a client cannot claim to be someone else by setting it.

### CORS, and why it is off

dup ships with **no CORS headers at all**, and a preflight from an unlisted origin is refused.

This is not an oversight. CORS is a browser mechanism, and nothing that calls dup is a browser. It cannot stop a request being made; a malicious page can already POST to your port. What actually stops that page is that dup requires an `Authorization` header, which is not CORS-safelisted, so the browser must preflight first, and dup answers no preflight. Adding permissive CORS would remove that protection rather than add one.

The knob exists only for the case where you later put a real browser UI in front of the API:

```yaml
cors:
  allowed_origins: ["https://dash.example.com"]
```

Exact origins only. `"*"` is rejected at config load.

## Architecture

Two processes, split on privilege.

```
  n8n / GitHub                    unix socket                  docker
  ──────────────▶       dup       ──────────▶     dup-agent      ──────────▶  compose
   HTTPS + basic    (unprivileged)  root:dup     (root, no network listener)
   auth via NPM     user: dup        0660
                    NO docker access
```

**`dup.service`** runs as the `dup` system account. It owns everything that touches attacker-controlled bytes: the HTTP listener, bearer-token and GitHub HMAC verification, JSON parsing, the job store, the auto-update scheduler, the outbound notify webhook. It has no Docker socket access and is not in the `docker` group.

**`dup-agent.service`** runs as root with no network listener at all. It accepts connections only on `/run/dup/agent.sock` (`root:dup`, mode `0660`) and does all the compose work.

This is enforced by what is compiled in, not only by configuration. The `dup` binary does not link the package that can execute `docker`, and a test asserts it:

```
$ go list -deps ./cmd/dup | grep internal/compose
$ go list -deps ./cmd/dup-agent | grep internal/compose
docker-updater/internal/compose
```

### Why not a user in the `docker` group

Because that is theatre. Docker socket access is root-equivalent:

```bash
docker run --rm -v /:/host --privileged alpine chroot /host sh
```

Anyone who can reach the socket is one command from root on the host and every container on it. A service account in the `docker` group looks safer on paper and buys nothing. The installer warns if `dup` ever ends up in that group.

The split is worth having, though not mainly because a memory-safety bug is likely. There is no cgo and no unsafe here, and the realistic bug in this codebase is an auth logic error, which the split does not help with. What it buys is structural: the dangerous capability is unreachable from the code that parses untrusted input, so the next person who adds an endpoint, a debug handler, or a "just let me pass a compose file" parameter is contained by architecture rather than by review vigilance.

Not using `sudo` is deliberate: `sudo` is setuid, which would force `NoNewPrivileges=true` off the unprivileged unit.

### What the split does not protect

- The agent is root and takes input over a socket the service account can write to. That is the trust boundary, and it is small on purpose: three endpoints, fixed JSON shapes with unknown fields rejected, a stack name looked up in the root-owned config, and a tag checked against the image-tag grammar. It re-validates everything rather than trusting the caller, holds its own per-stack lock, and caps concurrency.
- An attacker who lands as `dup` can still trigger redeploys of configured stacks. That is denial-of-service and a forced pull, not host compromise.
- **If the service account can write a stack's `docker-compose.yml` or its `.env`, the whole thing collapses.** It could add `privileged: true` and a bind mount of `/`, then ask the agent to deploy it as root. `.env` is just as dangerous, since compose reads it unconditionally and it can set `COMPOSE_FILE`, redirecting the agent at a different compose file entirely. Both are audited, and the audit is blocking.

### Security controls

| Control | What it does |
|---|---|
| Named stacks only | The request carries a stack name matched against the config. Directories, compose file names and service names live only in `/etc/dup/config.yml`. No parameter reaches a path or a command. |
| No shell | Every docker call is `exec` with an argv slice. Nothing is interpolated into a shell string. |
| Validated at load | `dir` must be absolute. `compose_file` and `env_file` must be bare filenames inside `dir`, and may not be symlinks pointing outside it. Stack and service names must match a strict regex, so a value can never be read as a docker flag. |
| Only one free parameter | `tag`, and only when the stack sets `image_tag_env`. Checked against the container image tag grammar by both processes. |
| Tag cannot change the image | Before applying a tag the agent resolves the stack with and without it and refuses if the repository moved, so `image: ${VERSION}` cannot be turned into "run whatever I name". |
| Unknown JSON fields rejected | On the public API and on the agent socket. |
| Loopback by default | Refuses to start on a non-loopback address unless `allow_non_loopback: true`. |
| Two inbound auth methods | Bearer token (constant-time compare of SHA-256 digests) and GitHub `X-Hub-Signature-256` HMAC over the raw body. After 20 failures in a minute the API returns 429 without sleeping, and a valid token does not clear that budget. |
| Peer credential check | On Linux the agent reads `SO_PEERCRED` and accepts only root and the configured `agent_peer_user`, on top of the socket mode. It fails closed. |
| Clean child environment | Docker is invoked with an explicit allowlisted environment, so `COMPOSE_FILE`, `COMPOSE_PROJECT_NAME` and `DOCKER_HOST` cannot be inherited into a compose call. |
| One update per stack | Enforced independently in both processes. |
| Notify URL is config-only | Never from a request, so the endpoint cannot be turned into an SSRF pivot. |
| Source allowlist | Optional `allow_from`, with `X-Forwarded-For` honoured only from configured `trusted_proxies`. |
| No CORS by default | A browser cannot use this API unless you name an origin explicitly. |

### File ownership, and the audit that enforces it

Nothing that decides what runs as root may be writable by the service account.

| Path | Owner | Mode |
|---|---|---|
| `/usr/bin/dup`, `/usr/bin/dup-agent` | `root:root` | `0755` |
| `/etc/dup/` | `root:dup` | `0750` |
| `/etc/dup/config.yml` | `root:dup` | `0640` |
| `bearer.token`, `github.secret` | `root:dup` | `0640` |
| every stack `dir`, its compose file, its `.env` | `root:root` | not writable by `dup` |
| every `pre_update.command` | `root:root` | not writable by `dup` |

That last row is the one that matters most and the one the installer cannot fix for you:

```bash
dup audit
```

It walks each stack's directory, compose file, configured env file and implicit `.env` **and every parent directory up to `/`**, since write access to a parent is enough to replace the file underneath it. Where a path is a symlink it walks both the link's ancestors and the resolved target's. It reports anything the service account can write and exits non-zero.

This audit is **blocking, not advisory**. `install.sh` refuses to start the services if it fails, and `dup-agent.service` re-runs it as an `ExecStartPre`, so ownership drift later stops the agent rather than silently handing it a compose file the service account controls.

## Configuration

```yaml
listen: "127.0.0.1:7788"
log_level: info

agent_socket: /run/dup/agent.sock
agent_peer_user: dup

auth:
  bearer_token_file: /etc/dup/bearer.token
  github_secret_file: /etc/dup/github.secret

defaults:
  check_interval: 6h
  soak: 30m
  pull_timeout: 10m
  health_timeout: 3m
  stability_window: 15s
  job_timeout: 25m
  rollback: true

notify:
  url: "https://n8n.example.com/webhook/dup"
  timeout: 15s
  headers:
    X-Source: dup

targets:
  - name: app
    dir: /opt/app
    compose_file: docker-compose.yml
    auto_update: true
    check_interval: 6h
    soak: 30m

  - name: api
    dir: /opt/api
    compose_file: docker-compose.yml
    services: [api]
    image_tag_env: APP_VERSION
```

| Key | Meaning |
|---|---|
| `dir` | Absolute path to the stack. Commands run here, so `.env` resolves exactly as it does by hand. |
| `compose_file` | Bare filename inside `dir`. Omit it and the usual four names are auto-detected. |
| `services` | Restrict the update to these services. Omit for the whole stack. This is also the health-check scope. |
| `auto_update` | Poll for a new image and apply it without anyone triggering it. |
| `check_interval` | How often to poll. Minimum one minute. |
| `soak` | How long a new image must have been available before it is applied. `0s` applies on sight. |
| `image_tag_env` | Enables the `tag` parameter. The value is exported to compose, so `image: repo:${APP_VERSION}` becomes settable per request. Without it, tags are refused. |
| `allow_prerelease` | Whether a GitHub pre-release triggers an update. Off by default. |
| `rollback` | Auto-rollback on a failed health check. On by default. |
| `health_timeout` | How long to wait for services to become healthy before treating the update as failed. |
| `stability_window` | How long they must stay healthy before the update is declared good. Stops a container that starts and then crashes a few seconds later from being reported as a success. |
| `pre_update` | Command to run before anything is recreated. See **Backups before an update**. |

Two directory-level rules worth knowing. `dir` must be absolute, and no two stacks may share a directory basename, because compose derives the project name from it and updating one would stop the other's containers. Both are refused at config load.

### Backups before an update

dup does not back up your volumes, and that is deliberate. A `tar` of a running Postgres volume is a torn copy that restores into a corrupt database while telling you that you are covered. Nothing in dup will ever create that file and call it a backup.

What it gives you instead is a **pre-update hook**: a command from the root-owned config that runs before anything is recreated.

```yaml
targets:
  - name: app
    dir: /opt/app
    pre_update:
      command: /usr/local/bin/backup-app
      args: ["--stack", "app"]
      timeout: 15m
      required: true
```

Point it at `pg_dump`, `restic`, a ZFS or LVM snapshot, or whatever you already trust and have actually restored from once.

- It runs as root, from the agent, with `cmd.Dir` set to the stack directory. No shell, so args are a list.
- The environment carries `DUP_STACK`, `DUP_DIR`, `DUP_TAG` and `DUP_SERVICES`.
- `required: true` (the default) means a non-zero exit **aborts the update** and nothing is touched.
- It runs only when there is genuinely something to apply, after the pull and the change comparison. A six-hourly auto-update check on an unchanged stack does not run your backup every six hours.
- The command is covered by `dup audit`, because a hook the service account can rewrite is root execution by another name.

Image rollback already covers the common failure, a bad release. The hook covers the one rollback cannot: a new version that ran a destructive migration, where rolling the image back leaves you on new schema with old code.

### Auto update, and why there is a soak

With `auto_update`, dup polls every `check_interval`, pulls, and compares what is running against what the registry now offers. When something new appears it does not apply it straight away: it records the time and waits out `soak`. Only if the image is still there after the soak does the update run.

That delay is the point. It means you are never the first host to run a broken release, and if you notice within the soak window you can pin a tag or turn auto update off before anything restarts. `soak: 0s` disables the wait and applies on sight.

If the new image is withdrawn or replaced during the soak, the timer clears on the next check and starts again next time it appears. A failed check never triggers an update and never resets a soak already in progress. Each stack's first poll is jittered over 90 seconds so a host with many stacks does not hit its registry all at once.

## API

All endpoints except `/healthz` need auth.

| Method | Path | Purpose |
|---|---|---|
| GET | `/healthz` | Liveness. No auth, no detail. |
| GET | `/v1/targets` | Configured stacks and whether each is busy |
| POST | `/v1/targets/{stack}/update` | Start an update |
| GET | `/v1/targets/{stack}/status` | Running job plus recent history for one stack |
| GET | `/v1/jobs?target=&limit=` | Recent jobs |
| GET | `/v1/jobs/{id}` | One job with its full step log |

```bash
curl -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"reason":"nightly check"}' \
  "https://deploy.example.com/v1/targets/app/update?wait=240s"
```

Body fields, all optional: `tag`, `reason`, `dry_run`, `force`.

`?wait=` blocks for up to 5 minutes and returns the finished job. Without it you get `202` immediately and poll `/v1/jobs/{id}`. With `wait`, a finished job returns `200` when it went well and `500` when it did not, so an n8n error branch works without inspecting the body.

### Response states

| State | Meaning |
|---|---|
| `no_change` | Images already current and everything healthy. Nothing was touched. |
| `succeeded` | Pulled, recreated, healthy for the full stability window. |
| `dry_run` | Validated and reported what it would do. |
| `failed` | Something went wrong before containers were replaced, or rollback was disabled. |
| `rolled_back` | The new images did not come up healthy. The previous images are running again. |
| `rollback_failed` | The update failed **and** the rollback failed. This one needs a human. |

### How rollback works

Before pulling, dup records the image ID each running container is using. If the health check fails afterwards, it points the image reference back at the recorded ID with `docker tag` and recreates. That restores the exact previous image even when the tag is mutable, like `:latest`. For stacks using `image_tag_env` it rolls back to the previous tag value, not merely the previous digest.

The one case it cannot save is a previous image that has already been pruned locally. The job message says so explicitly. If you run `docker image prune -a` on a schedule, you have no rollback safety net.

The desired image reference comes from `docker compose config`, not from `docker compose ps`, because `ps` reports the resolved image **ID** rather than the tag once a container has been recreated.

## Wiring it up

### Nginx Proxy Manager

| Field | Value |
|---|---|
| Domain | `deploy.example.com` |
| Scheme | `http` |
| Forward host | `127.0.0.1` |
| Forward port | `7788` |
| Websockets | off |
| Block common exploits | on |
| SSL | request a cert, force SSL, HTTP/2 |
| Access List | one with Basic Auth, satisfy `all` |

Basic auth on the proxy and the bearer token in dup are independent layers. Keep both: the proxy stops unauthenticated traffic reaching the process, and the token means a proxy misconfiguration is not on its own enough to trigger a deploy.

### n8n

Point an HTTP Request node at `/v1/targets/{stack}/update?wait=240s` with the bearer token as an `Authorization` header credential, and the proxy's basic auth as the node's own basic auth. Branch on the HTTP status, or on `ok` in the notify payload, to decide what to post to Discord.

For approval flows, put the Discord approval step before the HTTP Request node. dup has no concept of approval, deliberately: whatever can reach it can deploy. If you want an unattended path instead, use `auto_update` with a soak.

### GitHub webhook

Add a repository webhook pointing at `/v1/targets/{stack}/update`, content type `application/json`, secret set to the value in `/etc/dup/github.secret`, and send only the `release` event.

dup verifies the HMAC over the raw body and then filters: only `action: published` proceeds. Pings, drafts, and pre-releases (unless `allow_prerelease: true`) return `200` with `{"status":"ignored"}` so GitHub records a successful delivery instead of retrying. If the stack sets `image_tag_env`, the release tag is used as the image tag with any leading `v` stripped.

### Outbound

When `notify.url` is set, every finished job POSTs a summary there:

```json
{
  "host": "web01",
  "target": "app",
  "state": "rolled_back",
  "ok": false,
  "summary": "Rolled back app on web01: update failed, rolled back to the previous images",
  "trigger": "auto",
  "changed_services": ["app"],
  "duration_ms": 47213
}
```

`ok` is there so n8n can branch without parsing prose, and `summary` is ready to post straight to Discord. `trigger` is `api`, `github` or `auto`.

## Operating it

```bash
dup list
dup status app
systemctl status dup dup-agent
journalctl -u dup -u dup-agent -f
```

Logs are JSON on stdout, captured by journald. Job history is in memory only (last 200); journald is the durable record.

On `SIGTERM` each process stops accepting new work and waits for anything in flight, 30 seconds for the API and 90 for the agent.

The agent owns the lifetime of an update, not the caller's connection. If the API is restarted mid-deploy the agent carries on to completion or rollback on its own timeout rather than aborting half way. The API reports `failed` with "lost contact with the update agent" in that case, and `journalctl -u dup-agent` has the real outcome.

## Development

```sh
make help          # list every target
make check         # fmt-check, vet, golangci-lint, race tests
make build         # both binaries, with version metadata
make crosscheck    # compile every release target
make snapshot      # full release dry run into dist/
```

Requires Go 1.26.6. Dependencies are `gopkg.in/yaml.v3` and `golang.org/x/sys` (for `SO_PEERCRED`). Everything else is standard library.

### Releasing

Releases are cut by pushing a tag. GitHub Actions builds every artefact and attaches it to the release.

```sh
make check          # do not tag something that does not pass
make crosscheck     # do not tag something that does not cross-compile
git tag -a v1.2.3 -m "v1.2.3"
git push origin v1.2.3
```

The `Release` workflow then runs the tests again, and GoReleaser builds:

- `dup-linux-{amd64,arm64,armv7,386}.tar.gz`, each containing both binaries, `install.sh`, `deploy/` and the docs
- `.deb`, `.rpm` and `.apk` packages that install the units and create the service account
- `checksums.txt`
- a changelog grouped into Features, Fixes, Security and Other, from conventional commit prefixes

The version, commit and build date are injected at link time into `internal/version`, so `dup version` and `dup-agent -version` report exactly what was built. Nothing in the repo hardcodes a version.

Release targets are defined in exactly one place, the `goos`/`goarch`/`goarm` anchors on the first build in `.goreleaser.yaml`. The `dup-agent` build aliases them and `make crosscheck` compiles straight from that file, so the thing you test is by construction the thing you ship.

To rehearse a release without publishing, run the `Release` workflow manually from the Actions tab: it builds everything, publishes nothing, and uploads `dist/` as a workflow artefact. Locally, `make snapshot` does the same thing.

### CI

Every push and pull request runs:

| Job | What it checks |
|---|---|
| Test | `go test -race` |
| Lint | gofmt, tidy go.mod, vet, golangci-lint, shellcheck |
| Privilege split | that `cmd/dup` links no package able to execute docker |
| Cross-compile | every published target builds |
| Validate release config | `goreleaser check` |

The privilege-split job is not decoration. It is the one invariant that makes the unprivileged half meaningful, and a single stray import would silently remove it.

Two notes on the agent's systemd hardening, so nobody "improves" it into a broken state. `PrivateUsers=true` must never go on the agent unit: namespaced root cannot open `/var/run/docker.sock` and Docker stops working entirely. `ProtectSystem=strict` also breaks it, because the docker CLI writes state under `/root/.docker`; `full` is correct there, and `ProtectHome=read-only` rather than `true` is load-bearing because the CLI reads `/root/.docker/config.json` for private registry credentials.

And be honest about what that hardening achieves: every directive applies to the docker CLI process, not to `dockerd`, which lives outside the cgroup and does the actual work. It defends against a bug in the agent, and against nothing at all in "the agent asked dockerd for a privileged container". That is what the blocking audit is for.

## Licence

[GNU Affero General Public License v3.0 or later](LICENSE).

Copyright (c) 2026 PatchMon.

Worth understanding before you fork it: the AGPL's section 13 means that if you **modify** dup and let people interact with your modified version **over a network**, you have to offer those users the corresponding source. Running stock dup on your own servers triggers nothing, and neither does modifying it for purely internal use where you are the only one interacting with it. `dup version --full` prints the source URL so a downstream fork has somewhere to point.
