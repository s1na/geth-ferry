# geth-ferry

[![CI](https://github.com/s1na/geth-ferry/actions/workflows/ci.yml/badge.svg)](https://github.com/s1na/geth-ferry/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/s1na/geth-ferry.svg)](https://pkg.go.dev/github.com/s1na/geth-ferry)

A small Go tool that uploads and downloads geth datadir snapshots between a
node host and an S3-compatible object store. Replaces a manual
`tar | zstd | s3cmd` workflow with something that has a manifest, sha256
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
  parts/chaindata-live.tar.zst    # always: live pebble (SSTs, MANIFEST, WAL)
  parts/chaindata-live.toc.zst    # TOC sidecar: file list for the part above
  parts/ancient-chain.tar.zst     # always: chain freezer
  parts/ancient-chain.toc.zst     # TOC sidecar
  parts/ancient-state.tar.zst     # PBSS only: state freezer (account/storage)
  parts/ancient-state.toc.zst     # PBSS only: TOC sidecar
  parts/triedb.tar.zst            # PBSS only: merkle.journal
  parts/triedb.toc.zst            # PBSS only: TOC sidecar
```

`manifest.json` is written **last**, after every part has uploaded. No
manifest → the snapshot is incomplete and downloaders should treat it as
not-a-snapshot. Ferry refuses to upload if `chaindata/ancient/` contains
anything other than `chain/` and `state/` (fail-fast on geth versions we
don't understand).

Each part ships with a `.toc.zst` sidecar: a zstd-compressed text file
where every line is `<size> <name>` for one regular file inside the
part. The TOC's name, size, sha256, and entry count are recorded in the
manifest. The point is to make "what's in this snapshot?" answerable in
kilobytes of metadata, without ever fetching the multi-hundred-GiB part
itself; `ferry contents` reads only the manifest plus these sidecars.
A TOC is plain text, so `zstd -dc parts/<n>.toc.zst | head` is also a
sufficient ad-hoc inspector if you don't have ferry handy.

The auto-generated snapshot name is `geth-<chainid>-<role>-<block>`
(e.g. `geth-1-archive-23456789`); creation time lives in
`manifest.created_at`. **Operators are free to pass any path-safe string
via `--name`**: letters, digits, `-`, `.`, `_` are all fine; the only
constraint is "no slashes, no URL metacharacters, no whitespace". Names
no longer have to match the canonical shape. `ferry list` fetches each
snapshot's manifest to populate the chain/role/block/date columns, so
custom names render correctly there too.

Older ferry releases (≤ v0.1.0) appended a `-<unix-seconds>` tail for
collision-avoidance. That role is now filled by the upload-side check
that refuses to overwrite an existing snapshot unless `--overwrite` is
passed. v0.2.0+ neither generates nor parses the legacy 5-part form
(though such names are still reachable by URL, since ferry treats
custom names as opaque path segments).

## Upload

Stop geth first (ferry expects `<datadir>/geth/LOCK` and
`<datadir>/geth/geth.ipc` to be absent). Then:

```
AWS_ACCESS_KEY_ID=...
AWS_SECRET_ACCESS_KEY=...
ferry upload \
  --src /var/lib/geth \
  --dst 's3://my-bucket/snapshots/?endpoint=s3.example.com&region=us-east-1' \
  --role archive
```

`--name`, `--block`, and `--chain-id` are derived from the datadir
automatically (geth must be stopped; pebble's flock is exclusive).
You'll see a line like:

```
auto-detected: name=geth-1-archive-23456789 chain_id=1 head=23456789 state_scheme=path
```

before the upload starts. Pass any of the three explicitly to override.

Flags:

| Flag | Default | Notes |
|------|---------|-------|
| `--src` | (required) | datadir path (the dir containing `geth/`) |
| `--dst` | (required) | destination URL (see Backends below) |
| `--role` | (required) | `archive` or `full` (not derivable from on-disk state alone) |
| `--name` | auto | Free-form path-safe string; default is `geth-<chain>-<role>-<block>` |
| `--block` | auto | head block; read from rawdb if unset |
| `--chain-id` | auto | EVM chain id; read from rawdb chain config if unset |
| `--level` | `5` | zstd level (1-22; ≥ 10 forces single-threaded streaming in klauspost/compress) |
| `--threads` | GOMAXPROCS | zstd encoder threads |
| `--parallel-parts` | `1` | snapshot parts to upload concurrently (each part owns its own multipart buffer set; peak memory scales linearly; see Tuning) |
| `--multipart-size` | `0` (= 256 MiB) | S3 multipart part size, bytes. With S3's 10 000-part cap, the default caps a single object at ~2.5 TiB |
| `--multipart-concurrency` | `0` (= 5) | max in-flight UploadPart requests per object |
| `--force` | `false` | ignore preflight LOCK / .ipc check |
| `--overwrite` | `false` | replace an existing snapshot at the same `--name` (default: refuse with an error) |
| `--quiet` | `false` | suppress periodic progress output on stderr |
| `--dry-run` | `false` | print the planned upload (parts, source bytes, destination keys) and exit without writing anything |

Resume of an interrupted upload is **not** supported: rerun starts the
failed part over. Run inside `tmux` / `screen` to survive disconnects.
Ctrl+C is honored: in-flight S3 multipart uploads are aborted on the
remote so they don't squat as orphaned parts.

Use `--dry-run` to confirm what ferry plans to do (the auto-detected
name, the per-part source sizes and file counts, and the destination
keys) without committing to the multi-hour streamer. The walk runs all
parts in parallel.

Progress lines on stderr show throughput and (when `--dry-run` first
established the total) an ETA:

```
[chaindata-live] 124.32 GiB / 2.18 TiB (5.6%) ETA 3h41m in 12m38s (167.94 MiB/s)
```

## Download

```
ferry download \
  --src 's3://my-bucket/snapshots/geth-1-archive-23456789?endpoint=s3.example.com&region=us-east-1' \
  --dst /var/lib/geth
```

Each part is sha256-verified against the manifest as it streams.

The download command also reads **legacy single-file snapshots** when `--src`
ends in `.tar.lz4` or `.tar.zst`:

```
ferry download \
  --src 's3://my-bucket/archives/chaindata-23456789.tar.lz4?endpoint=s3.example.com&region=us-east-1' \
  --dst /var/lib/geth
```

Legacy snapshots have no manifest and no sha256; we trust the bytes the
backend returns.

Flags:

| Flag | Default | Notes |
|------|---------|-------|
| `--src` | (required) | snapshot URL (directory) or legacy single-file URL |
| `--dst` | (required) | datadir to extract into |
| `--force` | `false` | replace an existing `<dst>/geth/` (atomic; see below) |
| `--quiet` | `false` | suppress periodic progress output on stderr |
| `--parallel-parts` | `1` | snapshot parts to download concurrently (1 = sequential). Manifest snapshots only; ignored for legacy single-file URLs |

Extraction is atomic: parts land in `<dst>/.ferry-partial-*/` and are
renamed into place only after every part has been downloaded and
sha256-verified. A failed download leaves no partial state behind. With
`--force`, the original `<dst>/geth/` is removed *only at the moment of
promote*, never midway, so a mid-stream failure preserves your existing
datadir intact.

## Other commands

```
ferry inspect  <local-or-remote>            # print manifest.json (JSON output by default)
ferry list     --src <prefix-url>           # snapshots under a prefix (text columns; --json for scripts)
ferry verify   --src <snapshot-url>         # deep: re-fetch every part, recompute sha256
ferry verify   --src <snapshot-url> --quick # cheap: HEAD each part, compare size only
ferry contents --src <snapshot-url>         # list files inside the snapshot's parts (cheap)
```

`ferry list --json` emits an array of objects with one entry per
snapshot (`name`, `chain_id`, `role`, `block`, `timestamp`, `total_size`),
suitable for piping into `jq`.

`ferry verify` defaults to a *deep* check: every part is re-downloaded
and its sha256 recomputed against the manifest. For periodic monitoring
or pre-flight sanity checks where re-downloading TBs of data is too
expensive, `--quick` HEADs each part and compares the remote object's
size to `manifest.compressed_size` (kilobytes of metadata per part).
Quick mode catches "part missing or truncated"; only deep mode catches
silent corruption.

`ferry contents` reads only the manifest and the per-part TOC sidecars
(see Snapshot layout above); the parts themselves are never fetched, so
listing a 350 GiB snapshot answers in seconds. Snapshots produced by
older ferry versions that don't have TOCs are flagged in the output.

## Tuning

The S3 multipart upload uses two knobs that interact with host memory:

- `--multipart-size` (default 256 MiB): per-part buffer size.
- `--multipart-concurrency` (default 5): in-flight UploadPart requests
  per object.

Steady-state memory per `Put` is roughly `concurrency × multipart-size`
(~1.25 GiB at the defaults). Buffers are recycled via a `sync.Pool`, so
the figure is a ceiling, not a per-part allocation. With
`--parallel-parts N`, multiply by N. Each in-flight part owns its own
buffer set.

On the OVH m5.2xlarge with 16 GiB of RAM that ferry was developed
against, the default tuning is comfortable. Smaller hosts should lower
both, e.g.:

```
ferry upload ... --multipart-size $((64*1024*1024)) --multipart-concurrency 2
```

`--parallel-parts > 1` does not currently improve wall-clock for typical
mainnet snapshots because `chaindata-live` dominates total bytes; the
flag is plumbed end-to-end and worth experimenting with, but treat
default 1 as the steady-state choice until you've measured.

## Backends

Backends are dispatched by URL scheme:

### `s3://bucket/prefix/`

Uses AWS SDK v2 with optional endpoint override (for OVH and other
S3-compatible providers). Configurable via URL query string; credentials
come from the standard AWS chain (env vars, `~/.aws/credentials`, instance
profile).

Set `FERRY_S3_DEBUG=1` to enable verbose AWS SDK logging (request bodies,
response headers, retries) on stderr. Useful when diagnosing endpoint
compatibility issues.

| Query param | Default | Notes |
|-------------|---------|-------|
| `endpoint` | (AWS) | hostname or full `https://...` URL |
| `region` | (AWS_REGION) | bucket region |
| `path_style` | `true` | path-style addressing (`endpoint/bucket/key`); set `false` for virtual-hosted-style (`bucket.endpoint/key`, native AWS) |

Example for OVH:

```
s3://my-bucket/snapshots/?endpoint=s3.de.io.cloud.ovh.net&region=de
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
export FERRY_S3_TEST_URL='s3://my-bucket/ferry-test/?endpoint=s3.example.com&region=us-east-1'
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
go test ./pkg/backend/s3/ -run TestRoundTripIntegration -v
```
