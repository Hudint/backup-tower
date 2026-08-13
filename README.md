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

Milestones 1 to 4 of 5 are complete. The core loop works end to end: check,
snapshot, update, verify, and roll back when it goes wrong.

| | |
|---|---|
| ✅ M1 | Snapshot container configuration, named volumes and bind mounts |
| ✅ M2 | Restore and rollback |
| ✅ M3 | Selection engine and dry run |
| ✅ M4 | Update engine with health gate |
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
backup-tower plan                          # what would be acted on, and why
backup-tower plan --explain webapp         # the full reasoning for one container
backup-tower snapshot webapp               # snapshot, container keeps running
backup-tower snapshot webapp --stop always # stop first for a consistent copy
backup-tower snapshot webapp --binds       # include bind-mounted host paths
backup-tower list                          # what has been stored
backup-tower show webapp                   # details of the latest snapshot
backup-tower verify webapp                 # re-check archives against their checksums

backup-tower restore webapp                # put the data back
backup-tower rollback webapp               # data, configuration and image together

backup-tower update --dry-run              # what would be updated, per registry
backup-tower update                        # update everything that opted in
backup-tower update webapp                 # update exactly this one
backup-tower daemon                        # keep checking on an interval
```

`verify` matters more than it looks: a backup that is never read is only a
hypothesis.

Restores replace, they do not merge — anything written since the snapshot is
gone afterwards. Both commands print exactly what they are about to destroy and
ask before doing it; `--yes` skips the question, and without a terminal they
refuse rather than assume.

Archives are checksummed before anything is written. A snapshot that does not
match its manifest is refused, because a half-restored volume is worse than an
untouched broken one.

## Rollback

`rollback` is the full way back after a bad update: it restores the archived
data, recreates the container from its captured configuration, and puts it on
the image it was running when the snapshot was taken.

Three details make it hold up in practice:

**The old image is pinned.** After an update the previous image loses its tag,
which makes it fair game for `docker image prune`. Every snapshot tags it under
`backup-tower/keep` so it survives for as long as the snapshot does. Without
this the rollback path works right up until someone tidies up.

**Runtime state is separated from configuration.** The engine's inspect response
mixes the two, and handing the state back is how recreated containers end up
subtly wrong — a hostname frozen to a dead container's ID, DNS aliases pointing
at an ID that no longer exists, IP addresses already handed to someone else.

**The old container is moved aside, not deleted.** It is only removed once its
replacement exists; if anything fails in between, the original is put back under
its own name. A rollback that leaves you with no container at all would be a
worse failure than the one it was fixing.

## Updating

```
check the registry → pull → hooks → stop → snapshot → replace → health gate
                                                            ↓ failed
                                                        roll back
```

**The check is on manifest digests, not tags.** A moving tag like `:latest` says
nothing about whether the content changed, and pulling every image just to find
out is expensive enough on a busy host that people turn updates off. The engine's
own distribution endpoint answers the question without downloading anything.

**The pull happens before the container is stopped**, so the download never counts
as downtime. The container then goes down once — for the snapshot and the
replacement together — which is what makes the consistent snapshot free.

**Two strategies, chosen automatically.** A container that came from a compose
file is updated with `docker compose up -d --no-deps`, which is what its owner
would do by hand and what Komodo does when it redeploys. Everything else is
recreated through the API. If the compose plugin or the compose file is not
reachable, it falls back to the API path and says so rather than failing.

**A subtlety that decides whether updates are correct at all.** The engine's
inspect response does not distinguish what the operator asked for from what the
image supplied — `CMD`, `ENTRYPOINT`, `HEALTHCHECK`, `ENV` and `LABEL` all look
like they were requested. Recreating a container with that whole set pins the new
release to the *old* image's defaults. A release that changes its entrypoint runs
with the previous one, and — worst of all — a broken release passes the health
gate, because the healthcheck being evaluated is the old version's. backup-tower
therefore subtracts the old image's own defaults and keeps only what differs from
them.

### Health gate

If the image declares a `HEALTHCHECK`, that decides: the gate waits for healthy
and fails on unhealthy. Whether a healthcheck exists is read from the
configuration, not from whether the engine has populated the health state yet —
right after a start it has not, and treating that moment as "no healthcheck"
would let the weaker check decide a case the healthcheck was there to judge.

Without a healthcheck, the honest fallback is to watch for a crash loop over a
settle window, and the result says exactly that: *health verified only by staying
up*.

### Rollback

Automatic rollback is opt-in per container (`tower.rollback=true`). Undoing an
update means restoring data and discarding whatever the new version wrote —
right often enough to offer, not often enough to impose. When it is off, the
failure message names the exact command to run.

A rollback leaves the container on a **digest reference**, not a tag. The tag now
points at the release that just failed, so resolving it would immediately undo
the rollback; the next check reports `pinned to a digest` instead of quietly
looping.

### Hooks

`tower.hook.pre-update`, `.pre-snapshot` and `.post-update` run as `sh -c` inside
the container. Both pre-hooks run while the application is still up — pre-update
first as the coarse "a change is coming" signal, then pre-snapshot, so a dump is
taken with the application already quiesced.

**A failing pre-hook aborts the update before the container is touched.** If the
dump did not happen, the snapshot would look complete and not be.

## Selecting containers

**Automatic updates are opt-in.** Nothing is updated unless something says so,
and there is no built-in blocklist: which containers are safe to update is the
operator's decision, not the tool's.

`plan` is how you check that before enabling anything. On a host with no labels
set it must show nothing selected — and if it does not, that is exactly what you
want to find out beforehand:

```
$ backup-tower plan
CONTAINER      ACTION   STRATEGY  SNAPSHOT      SCHEDULE   ENABLED BY           NOTE
webapp         update   compose   always+binds  -          label: tower.enable
db             monitor  -         auto          0 4 * * *  rule: "the db"

102 containers, 1 would be updated, 1 monitored only, 1 scheduled backups
```

`--all` lists everything including the containers that were not selected,
`--explain <container>` prints the full reasoning for one of them.

Three sources feed the decision, in precedence order **label > rule file >
default**. Labels win because they sit on the container itself.

### Labels

| Label | Default | Meaning |
|---|---|---|
| `tower.enable` | `false` | Opt in to automatic updates |
| `tower.monitor-only` | `false` | Report available updates without applying them |
| `tower.snapshot` | `true` | Snapshot before updating |
| `tower.snapshot.binds` | `false` | Include bind-mounted host paths |
| `tower.snapshot.stop` | `auto` | `auto`, `always` or `never` |
| `tower.schedule` | — | Cron expression for backups independent of updates |
| `tower.retention.keep` / `.days` | `3` / `14` | Retention override |
| `tower.rollback` | `false` | Undo automatically when the health gate fails |
| `tower.strategy` | `auto` | `auto`, `compose` or `recreate` |
| `tower.hook.pre-snapshot` / `.pre-update` / `.post-update` | — | Command run inside the container |

Only the `tower.*` namespace is read. Labels belonging to other tools —
Watchtower's in particular — are deliberately ignored: acting on them would mean
touching containers that were marked for something else.

A misspelled `tower.*` label is reported as a problem rather than ignored. A typo
in the label that was meant to enable or protect a container otherwise stays
invisible until it matters.

### Rule file

For what labels cannot reach. See [`rules.example.yaml`](rules.example.yaml);
point `TOWER_RULES_FILE` at your copy. Rules are applied in order and later
matches override earlier ones, so broad-first, specific-last works naturally.
Unknown keys are rejected at load time.

### Komodo

Komodo is used purely as an additional selection source: it answers "which stacks
did the operator tag", and nothing else. Updates still go through the normal
path, which keeps the coupling to a single read call.

Tagged stacks resolve to local containers through their compose project name,
tagged deployments through their container name. Komodo manages several hosts
while backup-tower only sees its own, so set `KOMODO_SERVER` when a tag spans
more than one — without it you get a warning rather than a guess.

### What cannot be updated

Containers on locally built images have no registry to check, and containers
referencing an image only by ID have no name left to resolve. Both are reported
as such and skipped rather than failing on every run. Snapshots still work for
them.

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
| `TOWER_RULES_FILE` | — | Selection rules; absent is the normal case. |
| `KOMODO_URL` / `_API_KEY` / `_API_SECRET` / `_TAG` | — | Komodo as a selection source; all four required. |
| `KOMODO_SERVER` | — | Restrict to one Komodo server on multi-host setups. |
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
