# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-11-17

### Added

- Container runtime (`mkube run`): PID, mount, UTS, IPC and network
  namespaces, `pivot_root`, minimal `/dev`, cgroup v1 and v2 limits for
  memory, CPU and processes, OOM detection.
- Node agent (`mkube agent`): heartbeats, reconciliation, restart policies
  with exponential backoff (CrashLoopBackOff), per-pod logs over HTTP.
- Control plane (`mkube server`): REST API server with a persistent store and
  optimistic concurrency, filter-and-score scheduler, deployment controller
  with rolling updates and garbage collection, node controller that evicts
  pods from dead nodes.
- `minictl`: `apply -f`, `get`, `logs`, `scale`, `delete`, `version`.
- `make demo`: a 3-node cluster on one machine.
- CI: golangci-lint with NASA/JPL "Power of Ten" rules, race-detector unit
  tests, real-container integration tests, 65% coverage gate, govulncheck,
  CodeQL, Dependabot, tagged releases.
- `LEARNING.md`: six chapters explaining every part of the project.
- CI and releases build with the latest stable Go, so binaries never ship
  with known standard-library vulnerabilities.

### Fixed

- Containers started in parallel from the same image raced on a temporary
  `.oldroot` directory; the runtime now uses `pivot_root(".", ".")`.

[Unreleased]: https://github.com/MITRAKER/mini-kubernetes/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/MITRAKER/mini-kubernetes/releases/tag/v0.1.0
