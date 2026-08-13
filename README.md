# backup-tower

Automatic container updates that keep a way back.

Watchtower updates containers but saves nothing. When an update goes wrong — a
broken migration, an incompatible config, a bad image — the data in the volume
has already changed and there is no way back. backup-tower closes that gap.

The core idea: **during an update the container is stopped anyway.** That is
exactly the moment when volumes are cold and a consistent copy costs nothing —
no `pg_dump`, no `fsfreeze`, no application knowledge.

```
check image → stop container → snapshot (cold, consistent)
   → start with the new image → health gate → ok: done / failed: roll back
```

## Status

Milestone 1 of 5 is complete: snapshots work.

| | |
|---|---|
| ✅ M1 | Snapshot container configuration, named volumes and bind mounts |
| ⬜ M2 | Restore and rollback |
| ⬜ M3 | Selection engine and dry run |
| ⬜ M4 | Update engine with health gate |
| ⬜ M5 | Scheduling, retention, packaging |

## Storage format

Nothing proprietary. A snapshot is a directory of plain tar archives compressed
with zstd:

```
/backups/webapp/2026-08-13T14-22-05Z/
├── manifest.json     image digest, archive list, sizes, SHA256 per archive
├── spec.json         the untouched engine inspect response
├── volumes/pgdata.tar.zst
└── binds/_srv_webapp_data.tar.zst
```

If backup-tower is gone, broken or incompatible, `tar --zstd -xf` still gets the
data out. That property is worth more than any clever container format, and it
is covered by a test that extracts an archive with the system `tar` binary.

## Usage

```sh
backup-tower info                          # what engine am I talking to
backup-tower snapshot webapp               # snapshot, container keeps running
backup-tower snapshot webapp --stop always # stop first for a consistent copy
backup-tower snapshot webapp --binds       # include bind-mounted host paths
backup-tower list                          # what has been stored
backup-tower show webapp                   # details of the latest snapshot
backup-tower verify webapp                 # re-check archives against their checksums
```

`verify` matters more than it looks: a backup that is never read is only a
hypothesis.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `TOWER_BACKUP_DIR` | `/backups` | Destination. A bare path means a local directory. |
| `TOWER_HELPER_IMAGE` | — | Image for helper containers; required when volumes are not directly readable. |
| `TOWER_ZSTD_LEVEL` | `3` | Compression level. During an update, compression time is downtime. |
| `TOWER_CONCURRENCY` | `2` | Parallel snapshots. Disk throughput is the bottleneck. |
| `TOWER_RETENTION_KEEP` | `3` | Snapshots to keep per container. |
| `TOWER_RETENTION_DAYS` | `14` | Minimum age to keep. |
| `TOWER_INTERVAL` | `6h` | Update check interval. |
| `DOCKER_HOST` | platform default | Engine endpoint. |

## How volumes are read

A container cannot add mounts after it has started, so backup-tower running in a
container cannot mount foreign volumes into itself. It starts a short-lived
helper container instead — using **its own image**, because Go does tar and zstd
natively. No Alpine, no shell pipes, no external binaries, and the same
archiving code on both sides.

When the volume path is directly readable — running on the engine host with
sufficient rights — that faster path is used automatically.

## Running as a container

Compose directories must be mounted at an **identical path**. Compose creates
containers on the host daemon while resolving relative paths against our
filesystem; a different mount point silently breaks bind mounts.

```yaml
services:
  backup-tower:
    image: backup-tower:dev
    command: ["daemon"]
    environment:
      TOWER_HELPER_IMAGE: backup-tower:dev
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /etc/komodo/repos:/etc/komodo/repos:ro   # identical path, see above
      - /srv/backups:/backups
```

## Development

```sh
go test ./...                                        # unit tests
docker compose -f test/tower-test/compose.yaml up -d # throwaway stack
docker build -t backup-tower:dev .
```

The test stack covers the shapes that behave differently: a database in a named
volume, a bind-mounted host directory, a container with no storage at all, and
services with and without a `HEALTHCHECK`. Nothing publishes a port, so it
cannot collide with anything already running.

Never develop against production containers.
