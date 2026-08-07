# Changelog

All notable changes per release. Versions follow [semver](https://semver.org).

## v0.1.0 — 2026-08-07

First release. A Go process can profile itself and search its own logs, and get
the answer back as a value rather than on a port.

### Added

- **`pprofcapture` — live pprof profiles returned as a struct.** Eight kinds:
  `heap`, `allocs`, `goroutine`, `threadcreate` (instant snapshots), `cpu` and
  `trace` (windowed), plus `block` and `mutex`. `debug=0` returns base64 of the
  gzipped protobuf for `go tool pprof`; `debug>=1` returns text, and says so in
  the response so it is not fed to a parser that cannot read it.

  **Block and mutex profiling arm themselves.** Both are off by default in
  every Go process, which normally means contention is unprofilable unless you
  predicted you would need it. Passing `seconds > 0` enables the profiler for
  that window and disables it afterwards. The restore is exact:
  `runtime.SetBlockProfileRate` has no getter, so a capture resets the rate
  only if it was the one that set it nonzero, and the mutex fraction is
  restored to whatever preceded the call. Concurrent captures serialise,
  because both knobs are process-global.

  With `seconds = 0` the snapshot path runs and enables nothing — an empty
  profile is the honest answer rather than a silent enable.

- **`logsearch` — search the process's own in-memory log ring**, wrapping the
  ring from `slog-configurator`. Filters on structured attributes captured off
  the record rather than a substring of the formatted line, so it works the
  same in JSON or text mode and finds attributes bound upstream through
  `slog.Logger.With` that never appear on the record.

  Adds the parts a caller-facing surface needs: `TotalMatches` counted before
  paging (so a full page is distinguishable from the last page), a page size
  it will not exceed, and per-line truncation on a rune boundary so one
  enormous record cannot crowd out every other match or emit invalid UTF-8.

  A nil ring returns `ErrUnavailable`, not an empty result — "never wired" and
  "nothing matched" are different answers.

### Notes

- No transport is included, on purpose. Results are plain structs with `json`
  tags, so an admin route, an MCP tool, a CLI or an RPC can serve them without
  this library having an opinion about which.
- Nothing is enabled at rest: no listener, no background goroutine, no sampler.
- Dependencies are `common-go`, `ctxerrors` and `slog-configurator`. No
  Prometheus client, no MCP SDK, no `google/pprof`.
