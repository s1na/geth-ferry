# Uploading a pbss-archive snapshot to S3

This documents the one-shot procedure to take the live `pbss-archive` node's
chaindata, compress it, and upload it to the OVH S3 bucket as a public archive
snapshot. The companion file `.archive_snapshot_env.example` lists the env vars
referenced below.

## What this is for

The bench machines have a long-standing snapshot system under
`s3://geth-s3-storage/benchmarkers/` (loaded by `roles/ddir_restore`). Those are
small (49–203 GB), HBSS, and pre-merge.

This procedure produces a full **archive** snapshot:
- ~2.7 TB raw / ~2.0–2.2 TB compressed
- PBSS (path-based) state scheme
- Recent head (post-merge)
- Stored under a new prefix `archives/` to keep it separate from benchmarkers.

## Prerequisites

- **Keybase access** to `/keybase/team/ethereum.devops.bootnodes/geth_s3_bucket_s3cfg`
  (the `s3cmd` config — this is the only secret).
- **Teleport SSH access** to `pbss-archive.ethdevops.io` (see main `README.md`
  → "Archive node → Teleport setup").
- **Sudo on the host.**
- **`zstd` and `s3cmd` installed on the host** — the existing `setup.yaml` only
  installs `lz4 + s3cmd`, so you'll need to add `zstd` once:
  ```
  sudo apt-get install -y zstd
  ```
- **Free disk space**: none beyond what the running node already uses. The
  procedure streams `tar → zstd → s3cmd` and never lands the tarball on disk.

## Compression choice

`zstd -3 -T0`. Reasoning is in the chat decision log; short version: pebble
files inside chaindata are already snappy-compressed (lz4 gains ~5%), but the
`ancient/` freezer is uncompressed RLP and zstd takes 25–35% off it. Expect
~20–25% total size reduction over the raw datadir. `-T0` uses all cores and
sustains well above network bandwidth, so compression is never the bottleneck.

`lz4` is left in place for the existing `benchmarkers/` snapshots —
don't change those. The loader role will need a `.tar.zst` branch added before
anything can *consume* this archive (out of scope for this procedure).

## Naming

```
s3://geth-s3-storage/archives/archive-pbss-<block>-<YYYYMMDD>.tar.zst
```

- `<block>`: actual `eth_blockNumber` at the moment geth was stopped.
- `<YYYYMMDD>`: snapshot date.
- `pbss`: state scheme — required so future consumers know what they're loading.

## Procedure

All steps run **on `pbss-archive`** unless otherwise noted.

### 1. Pull the s3cmd config from keybase

From your local machine:

```
keybase fs read /keybase/team/ethereum.devops.bootnodes/geth_s3_bucket_s3cfg \
  | ssh pbss-archive 'sudo tee /etc/geth_s3cmd_config >/dev/null && sudo chmod 600 /etc/geth_s3cmd_config'
```

(Or copy any way you prefer — the file just needs to exist at `$S3CFG` on the host.)

### 2. Capture metadata before stopping geth

```
BLOCK_HEX=$(curl -s -H 'Content-Type: application/json' \
  -X POST --data '{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}' \
  http://127.0.0.1:8545 | jq -r .result)
SNAPSHOT_BLOCK=$((BLOCK_HEX))
SNAPSHOT_DATE=$(date -u +%Y%m%d)
echo "block=$SNAPSHOT_BLOCK date=$SNAPSHOT_DATE"
```

Note these values — they go into the filename.

### 3. Stop geth + blsync

```
sudo docker stop geth blsync
```

geth needs `stop_timeout: 120` worth of patience to flush pebble cleanly. Don't
kill -9.

### 4. Stream upload

Source `.archive_snapshot_env` (your filled-in copy of the `.example`), then:

```
S3_TARGET="s3://geth-s3-storage/archives/archive-pbss-${SNAPSHOT_BLOCK}-${SNAPSHOT_DATE}.tar.zst"

sudo tar -C /datadrive/geth/geth -cf - chaindata \
  | zstd -3 -T0 -c \
  | sudo s3cmd -c "$S3CFG" put - "$S3_TARGET" \
      --multipart-chunk-size-mb=512 \
      --no-progress
```

Notes:
- `-C /datadrive/geth/geth` so the tar paths start with `chaindata/...` (matches
  what the existing `ddir_restore` role expects on the consumer side).
- `--multipart-chunk-size-mb=512`: large parts to keep the multipart count under
  s3cmd's 10 000-part limit (a 2.2 TB upload at 512 MB/part = ~4 400 parts —
  comfortable).
- Run inside `tmux` / `screen`. Expect 9–12 hours.
- Don't pipe through `pv` unless you need it — extra mem copies, no real win.

### 5. Verify

```
s3cmd -c "$S3CFG" ls -H "$S3_TARGET"
```

Confirm size is in the expected ballpark (~2 TB).

### 6. Restart the node

```
sudo docker start geth blsync
```

Or just re-run the deploy playbook from your local machine:

```
ansible-playbook configure_archive.yaml -t deploy
```

## Time and cost estimate

- **Downtime**: ~9–12 hours, dominated by Hetzner→OVH upload bandwidth
  (~50–100 MB/s sustained cross-provider). Compression and disk read are not
  the bottleneck.
- **Egress (Hetzner side)**: 2.5 TB minus 1 TB free quota ≈ 1.5 TB × €1/TB = **~€1.50 one-time**.
- **Storage (OVH side, Performance tier)**: 2.0 TB × €18.25/TB-month ≈ **~€36/month** while it sits there.
- **Ingress**: free.

If this snapshot is going to live for more than a couple of months, consider
moving it to OVH Standard (~€7/TB-month) or Hetzner Object Storage (~€5/TB-month)
— see the cost comparison in chat history.

## Cleanup / sanity checks after the fact

- `sudo rm -f /etc/geth_s3cmd_config` if you don't want the cred sitting on the
  host afterwards (the existing `ddir_restore` role re-puts it when needed).
- Confirm `pbss-archive` is back in sync: `eth_blockNumber` should advance
  within a few minutes of restart.
