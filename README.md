# geth-ferry

A small Go tool that uploads and downloads geth datadir snapshots between a
node host and an S3-compatible object store. Replaces the manual
`tar | zstd | s3cmd` runbook in [`archive_snapshot_upload.md`](archive_snapshot_upload.md)
with something that has a manifest, sha256 verification, and a sane CLI.

The design plan lives in [`PLAN.md`](PLAN.md).

## Build

```
go build ./cmd/ferry
```

Requires Go 1.24+. No CGO.

## Snapshot layout

A snapshot is a directory at a known prefix on the remote:

```
s3://bucket/snapshots/<name>/
  manifest.json
  parts/chaindata.tar.zst
  parts/triedb.tar.zst       # only when <datadir>/geth/triedb/ exists (PBSS)
```

`manifest.json` is written **last**, after both parts upload. No manifest →
the snapshot is incomplete and downloaders should treat it as not-a-snapshot.

The snapshot name is `geth-<chainid>-<role>-<block>-<YYYYMMDD>` where
`role` ∈ `archive`, `full`. Example: `geth-1-archive-23456789-20260430`.

## Upload

Stop geth first (the runbook expects `<datadir>/geth/LOCK` and
`<datadir>/geth/geth.ipc` to be absent). Then:

```
AWS_ACCESS_KEY_ID=...
AWS_SECRET_ACCESS_KEY=...
ferry upload \
  --src /datadrive/geth \
  --dst 's3://geth-s3-storage/snapshots/?endpoint=s3.de.io.cloud.ovh.net&region=de' \
  --name geth-1-archive-23456789-20260430 \
  --role archive \
  --block 23456789 \
  --chain-id 1
```

Flags:

| Flag | Default | Notes |
|------|---------|-------|
| `--src` | (required) | datadir path (the dir containing `geth/`) |
| `--dst` | (required) | destination URL (see Backends below) |
| `--name` | (required) | `geth-<chain>-<role>-<block>-<YYYYMMDD>` |
| `--role` | (required) | `archive` or `full` |
| `--block` | (required) | head block at stop time |
| `--chain-id` | `1` | EVM chain id |
| `--level` | `5` | zstd level (1-22; ≥ 10 forces single-threaded streaming in klauspost/compress) |
| `--threads` | GOMAXPROCS | zstd encoder threads |
| `--force` | `false` | ignore preflight LOCK / .ipc check |
| `--quiet` | `false` | suppress periodic progress output on stderr |

Resume of an interrupted upload is **not** supported in v1: rerun starts the
failed part over. Run inside `tmux` / `screen` like the legacy runbook does.

## Download

```
ferry download \
  --src 's3://geth-s3-storage/snapshots/geth-1-archive-23456789-20260430?endpoint=s3.de.io.cloud.ovh.net&region=de' \
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
ferry inspect <local-or-remote>     # print manifest.json
ferry list    --src <prefix-url>    # tabular list of snapshots under a prefix
ferry verify  --src <snapshot-url>  # re-fetch each part and check sha256
```

## Backends

Backends are dispatched by URL scheme:

### `s3://bucket/prefix/`

Uses AWS SDK v2 with optional endpoint override (for OVH and other
S3-compatible providers). Configurable via URL query string; credentials
come from the standard AWS chain (env vars, `~/.aws/credentials`, instance
profile).

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
