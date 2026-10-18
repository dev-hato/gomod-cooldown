# Changelog

All notable user-visible changes to this project are documented here. The
project follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Stable `--help` and `--version` CLI behavior.
- Cross-platform CLI smoke tests and CI coverage for the minimum and current
  stable Go toolchains.
- Contributor guidance and an explicit v1 compatibility contract.

### Changed

- Recommend module-root updates with `go get -u -t ./...` followed by
  `go mod tidy`, avoiding `go get -u all` package-graph failures.
- Pin release-critical lint and vulnerability-analysis tooling in CI.
- License the project under MIT while retaining third-party fixture notices
  and license texts.

### Fixed

- Forward process signals and preserve meaningful child-process exit behavior.
- Parse fractional day durations consistently with `d` defined as exactly 24
  hours.
- Reject local proxy requests whose path contains `.` or `..` segments, or
  whose `Host` is not the loopback listener address.
- Fail discovery closed with 502 when a buffered upstream response
  (`@v/list`, `@latest`, `.info`, or `.mod`) exceeds 16 MiB, and fail index
  setup when an index page holds more records than requested.

## Earlier releases

Release notes for v0.1.0 through v0.6.0 are available on the
[GitHub Releases page](https://github.com/dev-hato/gomod-cooldown/releases).

[Unreleased]: https://github.com/dev-hato/gomod-cooldown/compare/v0.6.0...HEAD
