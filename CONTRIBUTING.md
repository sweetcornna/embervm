# Contributing to EmberVM

Thank you for your interest in EmberVM! The project is in a very early phase
(M0/M1 scaffolding), so the ground rules below matter even more than usual —
they keep the codebase legally clean and the history reviewable while things
move fast.

- Project license: **AGPL-3.0** (see [LICENSE](LICENSE))
- Governance & licensing policy source of truth: [docs/zh/05-开源项目规划.md](docs/zh/05-开源项目规划.md)
- Security issues: **do not open a public issue** — see [SECURITY.md](SECURITY.md)
- Community standards: [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)

## Contributor terms: DCO + CLA (dual-track)

EmberVM uses **both** a Developer Certificate of Origin and a Contributor
License Agreement:

1. **DCO — required on every commit.** Sign off each commit with

   ```bash
   git commit -s
   ```

   which appends a `Signed-off-by: Your Name <you@example.com>` trailer. By
   signing off you certify the [Developer Certificate of Origin 1.1](https://developercertificate.org/)
   — that you have the right to submit the change under the project license.
   Pull requests with unsigned commits will fail CI; fix with
   `git rebase --signoff`.

2. **CLA — required once, on your first pull request.** The CLA grants the
   project maintainers the right to relicense contributions. This preserves
   the project's **dual-licensing ability** (per docs/zh/05 §2): EmberVM is
   AGPL-3.0 for everyone, and the maintainers reserve the option to offer
   commercial licenses or a hosted edition in the future. A CLA bot will
   prompt you on your first PR; you only need to sign once.

If you cannot accept the CLA, please open an issue to discuss before writing
code — design feedback and bug reports are always welcome without one.

## Code-reuse compliance red lines

These are hard rules, not guidelines (docs/zh/05 §2):

- **Apache-2.0 code MAY be incorporated** into this AGPL-3.0 project (one-way
  compatible). Examples: firecracker-microvm, the Apache-licensed parts of
  e2b-dev/infra. When you do:
  - preserve the original copyright headers and NOTICE text, and
  - add an entry to [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) in the
    same pull request.
- **BSL-licensed code MUST NEVER be copied.** Parts of e2b-dev/infra are under
  the Business Source License. That code is a **read-only design reference**:
  you may study the architecture and re-implement ideas independently, but no
  code, comments, or non-trivial identifier structure may be copied or
  translated from it.
- When in doubt, ask in the PR/issue *before* importing anything. Provenance
  questions are cheap to answer up front and very expensive after a merge.

## Commit messages: Conventional Commits

All commits must follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/):

```
<type>(<optional scope>): <description>

[optional body]

Signed-off-by: Your Name <you@example.com>
```

Types: `feat`, `fix`, `docs`, `test`, `refactor`, `perf`, `build`, `ci`,
`chore`. Scopes typically match the package or area, e.g. `feat(uffd): ...`,
`fix(nodeagent): ...`, `docs(bench): ...`. Use `!` or a `BREAKING CHANGE:`
footer for breaking changes (the API may break during `v0.x`, but it must be
declared in the CHANGELOG).

## Development setup

Requirements:

- **Go 1.24** (module: `github.com/embervm/embervm`; the only allowed
  third-party Go dependency is `golang.org/x/sys` — propose anything else in
  an issue first)
- `shellcheck` for shell script linting
- Linux with `/dev/kvm` for real microVM runs (optional locally — see below)

Standard workflow:

```bash
make lint    # gofmt check + go vet + shellcheck
make build   # cross-compiles all binaries for linux/amd64 into bin/
make test    # unit tests (run anywhere, no KVM required)
```

### KVM and platform notes

Real microVM **integration tests only run on Linux with `/dev/kvm`** (bare
metal or nested virtualization). CI runs them automatically: GitHub Actions
`ubuntu-latest` runners expose `/dev/kvm`, so every pull request gets real
Firecracker boots for free.

Contributors on **macOS** (or Linux without KVM) can develop comfortably with
`make lint` and `make test` — all components are designed to be mockable
without KVM — and rely on CI for the real-microVM integration suite. The
Makefile cross-compiles Linux binaries from macOS out of the box. If you want
to run the full suite yourself, see the verified cloud-server matrix in
[docs/zh/06-云服务器实测指南.md](docs/zh/06-云服务器实测指南.md) (a $6/mo
DigitalOcean droplet is enough for smoke tests).

## Architecture decisions: ADRs

Significant architecture decisions are recorded as Architecture Decision
Records under `docs/adr/` (the conclusions of the Chinese design docs 01–04
are being distilled into ADR-0001..N). If your change alters an architectural
choice — snapshot format, tiering policy, isolation model, external
dependencies — include a new or updated ADR in the same PR. Small, local
implementation choices do not need an ADR.

## Finding something to work on

- Issues labeled **`good-first-issue`** are curated to be self-contained and
  well-scoped — start there.
- The roadmap (M0–M5) is mirrored as GitHub Milestones; each milestone issue
  links to the relevant design doc section.
- Before starting anything non-trivial, comment on the issue (or open one) so
  work isn't duplicated.

## Pull request checklist

- [ ] Every commit is signed off (`git commit -s`) and follows Conventional Commits
- [ ] `make lint`, `make build`, and `make test` pass locally
- [ ] New third-party code (Apache-2.0 only) has attribution + a
      `THIRD_PARTY_NOTICES.md` entry; no BSL-derived code anywhere
- [ ] Architecture-level changes include an ADR under `docs/adr/`
- [ ] Docs updated where behavior changed (English primary)
