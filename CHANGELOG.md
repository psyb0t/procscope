# Changelog

All notable changes per release. Versions follow [semver](https://semver.org).

## v0.3.1 — 2026-08-08

Repository infrastructure only. No library code changed.

- Added the imported-by badge: a count of the public packages importing this
  module, linking to `importers.md` on the `badges` branch — the importing
  repositories, grouped, package counts descending, and flagged when the owner
  differs from this repo's.
- It measures **blast radius, not adoption**. The number tells you how much
  breaks when an exported name moves; the external mark tells you whether any of
  that is someone else's problem, which is what decides how strictly the module
  has to be versioned.
- **It will read `unknown` until pkg.go.dev crawls this module**, which is
  expected for a repo published days ago. That is deliberate rather than a bug:
  "nothing imports this" and "I could not tell" are different facts, and
  rendering the second as a confident `0` would be worse than saying nothing.
- Refreshed weekly rather than daily, because pkg.go.dev's crawl lags
  publication by days and each run drags the full test suite along (the badges
  job needs the coverage artifact). The whole pipeline runs rather than a
  badges-only job: the badge publisher republishes only what a run produced, so
  a badge-only refresh would delete the coverage, version and license badges.
- The cron slot is derived from a hash of the repository name rather than
  chosen — GitHub cron has no randomness, and its scheduler sheds queued runs
  hardest at the round times a human would pick.

## v0.3.0 — 2026-08-08

Typed options instead of a magic integer, and the package moves to the repo
root.

### Changed

- **`pprofcapture` is now the root package.** `pprofcapture.Capture(...)`
  becomes `procscope.Capture(...)` — one import path, no stutter.

- **`Capture(kind string, debug int, seconds int)` is now
  `Capture(Options)`,** and every axis is a named type:

  ```go
  result, err := procscope.Capture(procscope.Options{
      Kind:   procscope.KindGoroutine,
      Format: procscope.FormatText,
      Detail: procscope.DetailGrouped,
      Window: 10 * time.Second,
  })
  ```

  The zero `Options{}` is a heap profile in protobuf form — the common case —
  so the simplest call is `Capture(Options{})`.

- **`debug int` is gone, split into `Format` and `Detail`.** It was two
  unrelated decisions in one number, inherited from
  `runtime/pprof.Profile.WriteTo`: `0` meant protobuf, and anything higher
  meant text AND picked a verbosity. Now `Format` chooses `FormatProto` or
  `FormatText`, and `Detail` chooses `DetailGrouped` (identical stacks
  collapsed with a count — what finds a goroutine leak) or `DetailFull`. The
  pprof debug integer is no longer part of the API at all.

  `Detail` applies only to `FormatText`; `Result.Detail` comes back empty for
  protobuf rather than echoing a value that had no effect.

- **`seconds int` is now `Window time.Duration`.** No more guessing the unit.
  `DefaultWindow` (5s) and `MaxWindow` (30s) replace the bare numbers.

- **`Kind` is a named type**, so `KindHeep` is a compile error instead of
  reaching the runtime as a silent miss. The values still match the names the
  Go runtime registers profiles under.

- **`Encoding` is a named type** (`EncodingBase64Gzip` / `EncodingText`).

### Fixed

- **An unrecognised `Kind`, `Format` or `Detail` is now an error** rather than
  a silent fallback to heap. A typo that quietly returns the wrong profile
  costs more than one that says so, and the message names the offending value.

- **`FormatText` on `KindCPU` or `KindTrace` is rejected up front.** Neither
  has a text form, so the previous behaviour would have produced a `Result`
  whose `Format` contradicted its `Encoding`.

## v0.2.1 — 2026-08-08

Repo plumbing only. No library code changed.

- The repo is now **mirrored to GitLab and Codeberg** on every push, and
  **archived to the Wayback Machine, Software Heritage and archive.org**. The
  archive runs only for the default branch, tags, the monthly cron and manual
  dispatch, since Save Page Now is rate-limited.
- Issues opened on either mirror are **pulled back into the GitHub tracker**
  on a six-hourly schedule. The scheduled run jitters so both mirrors are not
  hit by every repo at once; a manual dispatch runs immediately.
- Pull requests from non-collaborators are **closed and locked** with a
  pointer to the issue tracker.
- `pre-commit.sh` runs `make lint && make test-coverage`.

## v0.2.0 — 2026-08-08

`logsearch` is gone. It wrapped a type that already did the job.

### Removed

- **`logsearch`.** It existed to search a process's in-memory log ring, but
  `logring.Handler` in [slog-configurator](https://github.com/psyb0t/slog-configurator)
  already **is** the `slog.Handler` and already exposes `Search`, `Count`,
  `Tail`, `Clear` and `Stats`. There was nothing left to wrap:

  ```go
  ring := logring.New(logring.Options{})
  slogconfigurator.AddHandler(ring)   // the ring IS the handler

  entries := ring.Search(logring.SearchOptions{Contains: "timeout"})
  ```

  What the package added on top — a page-size clamp, per-line truncation, a
  match count computed before paging — belongs on the ring itself, where every
  consumer benefits, rather than behind a wrapper only this library's users
  could reach. Its filter type was already a straight alias of the ring's, so
  nothing about the query surface is lost.

  **If you used it:** replace `logsearch.New(logsearch.Config{...})` with
  `logring.New(logring.Options{...})`, pass the ring straight to `AddHandler`,
  and call `Search` on it. The filter fields are identical. The extras —
  clamping, truncation, total-before-paging — are not yet upstream; clamp and
  truncate at your call site until they are.

- **The `slog-configurator` dependency**, which existed only for `logsearch`.
  procscope's dependencies are now `common-go` and `ctxerrors`.

### Added

- **`pprofcapture/README.md`** — a full walkthrough rather than a fragment:
  complete runnable programs, an HTTP handler that serves profiles, what to
  actually type to open one in `go tool pprof`, every profile kind and what it
  answers, every field on `Result`, and recipes for the four questions people
  reach for this with.

### Changed

- The root README's log-search example was missing its import block entirely,
  so it could not be copied and run. It is replaced by a working example that
  points at `logring` directly.

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
