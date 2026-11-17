# Security policy

Mini Kubernetes is a learning project and is **not intended for production
use**. In particular, its API has no authentication and the runtime must run
as root.

## Reporting a vulnerability

Please do not open a public issue. Use GitHub's private
[security advisory form](https://github.com/MITRAKER/mini-kubernetes/security/advisories/new)
instead. You can expect an acknowledgement within a week.

## How the project checks itself

- `gosec` (through golangci-lint) on every pull request.
- `govulncheck` for known vulnerabilities in dependencies and the standard
  library, on every pull request.
- CodeQL analysis on every pull request and weekly.
- Dependabot keeps Go modules and GitHub Actions up to date.
