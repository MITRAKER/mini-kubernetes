# Contributing

Thanks for your interest in Mini Kubernetes! This is a learning project, so
contributions that make the code clearer or teach something new are as
welcome as new features.

## Getting started

You need Linux, the latest stable Go release (the code needs 1.23 or newer, but
older toolchains have known standard-library vulnerabilities that
`govulncheck` reports) and, for the integration tests, root.

```bash
git clone https://github.com/MITRAKER/mini-kubernetes
cd mini-kubernetes
make tools          # installs golangci-lint, govulncheck and pre-commit hooks
make check          # format, lint, unit tests: what CI runs on every PR
sudo make test-integration
```

## Coding standard

The code follows the **NASA/JPL "Power of Ten" rules, adapted to Go**. They
are explained in [LEARNING.md, chapter 6](LEARNING.md#chapter-6-professional-quality-and-the-nasajpl-rules)
and enforced by `golangci-lint` (see `.golangci.yml`). In short:

1. Simple control flow: no deeply nested code, cyclomatic complexity ≤ 12.
2. Every loop is bounded, in count or in time (a cancellable context).
3. Bounded resources: limit input sizes, set timeouts on every wait.
4. Functions fit on a page: at most 60 lines and 40 statements.
5. Check preconditions at public entry points and return errors.
6. Declare variables in the smallest possible scope.
7. Check every returned error. Ignoring one must be explicit (`_ =`) and
   carry a comment saying why it is safe.
8. No code generation or build tags; keep reflection to a minimum.
9. No `unsafe` outside the one audited `ioctl` in `internal/runtime`.
10. Zero warnings: lint, vet and tests must pass with no findings.

A `//nolint` directive needs a comment that names the rule and the reason.

## Pull requests

- One topic per pull request, with tests for any behaviour change.
- Update `CHANGELOG.md` under **Unreleased**.
- If you add a concept, add a few lines to `LEARNING.md` explaining it.
- Commit messages: a short imperative summary line ("Add node selectors"),
  then a blank line and the why.

CI must be green before merging: lint, unit tests, integration tests with
real containers, coverage of at least 65%, govulncheck and CodeQL.

## Releasing

1. Move the **Unreleased** entries in `CHANGELOG.md` under a new version.
2. Tag and push: `git tag v0.2.0 && git push origin v0.2.0`.
3. The release workflow builds Linux binaries and publishes the release.

Versions follow [Semantic Versioning](https://semver.org).
