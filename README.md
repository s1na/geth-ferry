# geth-ferry

[![CI](https://github.com/s1na/geth-ferry/actions/workflows/ci.yml/badge.svg)](https://github.com/s1na/geth-ferry/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/s1na/geth-ferry.svg)](https://pkg.go.dev/github.com/s1na/geth-ferry)

A small Go tool that uploads and downloads geth datadir snapshots between a
node host and an S3-compatible object store. Replaces a manual
`tar | zstd | s3cmd` runbook with something that has a manifest, sha256
verification, and a sane CLI.

The longer design write-up lives in [`docs/design.md`](docs/design.md).

## Build

Requires Go 1.24+. No CGO.

```
make build              # produces ./ferry, version derived from `git describe`
make build VERSION=0.2.0
```

Or, without make:

```
go build -ldflags "-X main.version=$(git describe --tags --always --dirty)" ./cmd/ferry
```

## Snapshot layout

A snapshot is a directory at a known prefix on the remote:

```
s3://bucket/snapshots/<name>/
  manifest.json
  parts/chaindata-live.tar.zst    # always — live pebble (SSTs, MANIFEST, WAL)
  parts/ancient-chain.tar.zst     # always — chain freezer
  parts/ancient-state.tar.zst     # PBSS only — state freezer (account/storage)
  parts/triedb.tar.zst            # PBSS only — merkle.journal
```

`manifest.json` is written **last**, after every part has uploaded. No
manifest → the snapshot is incomplete and downloaders should treat it as
not-a-snapshot. Ferry refuses to upload if `chaindata/ancient/` contains
anything other than `chain/` and `state/` — fail-fast on geth versions we
don't understand.

The snapshot name is `geth-<chainid>-<role>-<block>-<unix-seconds>` where
`role` ∈ `archive`, `full`. The trailing component is a Unix timestamp,
matching `manifest.created_at`. Example: `geth-1-archive-23456789-1746014400`.

## Upload

Stop geth first (the runbook expects `<datadir>/geth/LOCK` and
`<datadir>/geth/geth.ipc` to be absent). Then:

```
AWS_ACCESS_KEY_ID=...
AWS_SECRET_ACCESS_KEY=...
ferry upload \
  --src /datadrive/geth \
  --dst 's3://geth-s3-storage/snapshots/?endpoint=s3.de.io.cloud.ovh.net&region=de' \
  --role archive
```

`--name`, `--block`, and `--chain-id` are derived from the datadir
automatically (geth must be stopped — pebble's flock is exclusive).
You'll see a line like:

```
auto-detected: name=geth-1-archive-23456789-1746014400 chain_id=1 head=23456789 state_scheme=path
```

before the upload starts. Pass any of the three explicitly to override.

Flags:

| Flag | Default | Notes |
|------|---------|-------|
| `--src` | (required) | datadir path (the dir containing `geth/`) |
| `--dst` | (required) | destination URL (see Backends below) |
| `--role` | (required) | `archive` or `full` (not derivable from on-disk state alone) |
| `--name` | auto | `geth-<chain>-<role>-<block>-<unix-seconds>` |
| `--block` | auto | head block; read from rawdb if unset |
| `--chain-id` | auto | EVM chain id; read from rawdb chain config if unset |
| `--level` | `5` | zstd level (1-22; ≥ 10 forces single-threaded streaming in klauspost/compress) |
| `--threads` | GOMAXPROCS | zstd encoder threads |
| `--force` | `false` | ignore preflight LOCK / .ipc check |
| `--quiet` | `false` | suppress periodic progress output on stderr |
| `--dry-run` | `false` | print the planned upload (parts, source bytes, destination keys) and exit without writing anything |

Resume of an interrupted upload is **not** supported in v1: rerun starts the
failed part over. Run inside `tmux` / `screen` like the legacy runbook does.

Use `--dry-run` to confirm what ferry plans to do — the auto-detected name,
the per-part source sizes and file counts, and the destination keys —
without committing to the multi-hour streamer.

## Download

```
ferry download \
  --src 's3://geth-s3-storage/snapshots/geth-1-archive-23456789-1746014400?endpoint=s3.de.io.cloud.ovh.net&region=de' \
  --dst /datadrive/geth
```

Each part is sha256-verified against the manifest as it streams.

The download command also reads **legacy single-file snapshots** when `--src`
ends in `.tar.lz4` or `.tar.zst`:

```
ferry download \
  --src 's3://geth-s3-storage/archives/chaindata-23456789.tar.lz4?endpoint=s3.de.io.cloud.ovh.net&region=de' \
  --dst /datadrive/geth
```

Legacy snapshots have no manifest and no sha256; we trust the bytes the
backend returns.

Flags:

| Flag | Default | Notes |
|------|---------|-------|
| `--src` | (required) | snapshot URL (directory) or legacy single-file URL |
| `--dst` | (required) | datadir to extract into |
| `--force` | `false` | extract into a non-empty `<dst>/geth/` |
| `--quiet` | `false` | suppress periodic progress output on stderr |

## Other commands

```
ferry inspect  <local-or-remote>     # print manifest.json
ferry list     --src <prefix-url>    # tabular list of snapshots under a prefix
ferry verify   --src <snapshot-url>  # re-fetch each part and check sha256
ferry contents --src <snapshot-url>  # list files inside the snapshot's parts (cheap)
```

`ferry contents` reads the manifest plus a small sidecar (`parts/<n>.toc.zst`)
written alongside each part at upload time — typically a few KB to a couple
of MB total. It does **not** read the parts themselves, so listing a 350 GB
snapshot answers in seconds. Snapshots produced by older ferry versions
that don't have TOCs are flagged in the output.

## Backends

Backends are dispatched by URL scheme:

### `s3://bucket/prefix/`

Uses AWS SDK v2 with optional endpoint override (for OVH and other
S3-compatible providers). Configurable via URL query string; credentials
come from the standard AWS chain (env vars, `~/.aws/credentials`, instance
profile).

Set `FERRY_S3_DEBUG=1` to enable verbose AWS SDK logging (request bodies,
response headers, retries) on stderr — useful when diagnosing endpoint
compatibility issues.

| Query param | Default | Notes |
|-------------|---------|-------|
| `endpoint` | (AWS) | hostname or full `https://...` URL |
| `region` | (AWS_REGION) | bucket region |
| `path_style` | `true` | path-style addressing (`endpoint/bucket/key`); set `false` for virtual-hosted-style (`bucket.endpoint/key`, native AWS) |

Example for OVH:

```
s3://geth-s3-storage/snapshots/?endpoint=s3.de.io.cloud.ovh.net&region=de
```

For native AWS S3, disable path-style addressing:

```
s3://my-bucket/snapshots/?region=us-east-1&path_style=false
```

### `file:///abs/path`

Local filesystem. Used for tests and for staging snapshots to a different
mount before uploading. Uploads use atomic rename, so an interrupted upload
leaves no partial file visible.

## Testing

The default test suite is hermetic:

```
go test ./...
```

To exercise the S3 backend against a real bucket, set `FERRY_S3_TEST_URL`:

```
export FERRY_S3_TEST_URL='s3://my-bucket/ferry-test/?endpoint=s3.de.io.cloud.ovh.net&region=de'
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
go test ./pkg/backend/s3/ -run TestRoundTripIntegration -v
```
