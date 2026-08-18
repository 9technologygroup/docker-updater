# Contributing

Thanks for looking. dup is a small, deliberately boring tool that runs as root on other
people's servers, so the bar is correctness over features.

- [Before you start](#before-you-start)
- [Development](#development)
- [What gates a merge](#what-gates-a-merge)
- [Writing tests](#writing-tests)
- [Commit messages](#commit-messages)
- [Releasing](#releasing)

---

## Before you start

**Open an issue first for anything non-trivial.** A bug fix with a test can go straight to a
pull request. A new feature, a new dependency or a change to the privilege split is worth
agreeing before you spend an evening on it.

Two things that will be asked of any change:

- **Does it need a new dependency?** dup has two, `gopkg.in/yaml.v3` and `golang.org/x/sys`.
  That is a feature. A change that adds a third needs to justify itself.
- **Does it touch the privilege split?** The unprivileged binary must never gain the ability
  to execute `docker`, directly or through a transitive import. CI enforces this, and the
  check is not decoration.

---

## Development

```sh
make help          # list every target
make check         # fmt-check, vet, golangci-lint, race tests
make build         # both binaries, with version metadata
make crosscheck    # compile every release target
make snapshot      # full release dry run into dist/
```

Requires Go 1.26.6. Dependencies are `gopkg.in/yaml.v3` and `golang.org/x/sys` (for `SO_PEERCRED`). Everything else is standard library.

## Two things not to "improve"

`PrivateUsers=true` must never go on the agent unit: namespaced root cannot open `/var/run/docker.sock` and Docker stops working entirely. `ProtectSystem=strict` also breaks it, because the docker CLI writes state under `/root/.docker`; `full` is correct there, and `ProtectHome=read-only` rather than `true` is load-bearing because the CLI reads `/root/.docker/config.json` for private registry credentials.

And be honest about what that hardening achieves: every directive applies to the docker CLI process, not to `dockerd`, which lives outside the cgroup and does the actual work. It defends against a bug in the agent, and against nothing at all in "the agent asked dockerd for a privileged container". That is what the blocking audit is for.

---

## Releasing

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

## What gates a merge

Everything below runs in CI on every push and pull request. Run it locally first:

```sh
make check      # gofmt -s, go vet, golangci-lint, go test -race
make crosscheck # compile every published target
shellcheck -S warning install.sh packaging/*.sh
```

| Job | What it checks |
|---|---|
| Test | `go test -race` |
| Lint | gofmt, tidy `go.mod`, vet, golangci-lint, shellcheck |
| Privilege split | that `cmd/dup` links no package able to execute docker, and `cmd/dup-agent` links no update checker |
| Installer secrets | that no installer script prints a secret value |
| Cross-compile | every published target builds |
| Validate release config | `goreleaser check` |

The privilege-split job is the one to care about. It is the invariant that makes the
unprivileged half meaningful, and a single stray import would remove it silently.

---

## Writing tests

The house style, which reviewers will look for:

**A regression test must fail against the bug it describes.** Write it, watch it fail against
the old behaviour, then fix. A test that passes before and after proves nothing, and several
in this repo exist because that check was actually run.

**Name the behaviour, not the function.** `TestSoakAppliesWithoutWaitingForTheNextCheck` says
what breaks. `TestWatch` does not.

**Say why in a comment when the reason is not obvious.** Several tests here carry a line
explaining the production failure that motivated them, so nobody deletes them as redundant.

**Table-driven where there is a matrix**, plain where there is not. Use `t.TempDir()` and
`httptest`; nothing should touch a real Docker daemon, a real registry or the network.

---

## Commit messages

Conventional prefixes, because the changelog is generated from them:

```
feat:  a new capability
fix:   a defect
docs:  documentation only
test:  tests only
chore: tooling, dependencies, CI
```

`docs:`, `test:`, `chore:` and `ci:` are filtered out of the release notes, so use them
accordingly.

Write the body for someone reading it in a year with no memory of the conversation. Say what
was wrong and why the fix is the right shape, not just what changed. The diff already says
what changed.

---

## Code style

- No essays in comments. Default to none. Write one only where a future reader would
  otherwise revert or break something, and keep it to a line or two.
- Handle every error. `errcheck` will fail the build otherwise.
- Handlers go through a store; nothing reaches out to Docker except the agent.
- UK English in prose and user-facing strings.

---

## See also

- [DOCUMENTATION.md](DOCUMENTATION.md) for how the thing actually works
- [SECURITY.md](SECURITY.md) for the privilege split you must not break
