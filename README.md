# backup-tower

**Automatic Docker container updates that keep a way back.** Like Watchtower, but
it snapshots your volumes and configuration before every update — so a bad
release can be undone.

Watchtower updates containers and saves nothing. When an update goes wrong — a
broken migration, an incompatible config, a bad image — the data in the volume
has already changed and there is no way back. backup-tower closes that gap.

The core idea: **during an update the container is stopped anyway.** That is
exactly the moment when volumes are cold and a consistent copy costs nothing —
no `pg_dump`, no `fsfreeze`, no application knowledge.

```
check the registry → pull → stop → snapshot → replace → health check
                                                    ↓ failed
                                                roll back
```

Your backups are plain `tar` archives compressed with zstd. If this tool is gone,
broken, or a version that cannot read its own older output, `tar --zstd -xf`
still gets your data out — and a test extracts an archive with the system `tar`
binary so that cannot quietly stop being true.

> **An update broke something?** Jump to [Recovery](#recovery).

Feature complete, pre-1.0. Docker on Linux. No published image yet —
you build it, in one command, below.

---

## Requirements

- **Linux**, with Docker (engine API 1.40 or newer; tested against 29.x)
- Podman also works in principle: it serves the same API. Untested — reports
  welcome.
- Access to the container socket, normally `/var/run/docker.sock`
- Disk for the backups: roughly the compressed size of your volumes × how many
  snapshots you keep. A 47 MB PostgreSQL volume compresses to about 4 MB.
- To build: Docker, or Go 1.24+

> Mounting the Docker socket grants control of the engine, which is equivalent to
> root on the host. That is true of Watchtower, Portainer and every other tool in
> this category, but it is worth saying out loud.

## Install

There is no published image yet. Build it:

```sh
git clone https://github.com/Hudint/backup-tower.git
cd backup-tower
docker build -t backup-tower:latest .
```

Or, if you would rather have a plain binary on the host:

```sh
go build -o backup-tower ./cmd/backup-tower
```

The binary is static and needs nothing at runtime. The container image is based
on `docker:cli` because it also carries the compose plugin, which the compose
update strategy shells out to.

## First five minutes

Nothing is touched until you ask for it. Work through this and you end with a
snapshot you have verified and unpacked yourself.

**1. Check it can reach your engine.**

```sh
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
  backup-tower:latest info
```

```
backup-tower  0.1.0
engine        docker 29.4.3
api           1.54
host          unix:///var/run/docker.sock
containers    144 total, 102 running
```

**2. Ask what it would do.** On a fresh install the answer must be "nothing":

```sh
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
  -v /srv/backups:/backups backup-tower:latest plan
```

```
Nothing selected. 102 containers were evaluated and none opted in.
Set the label tower.enable=true on a container, or use a rule file, to enable it.
```

**3. Snapshot one container by hand.** Pick something unimportant. `--stop always`
stops it while its data is read, which is what makes the copy consistent; for a
small volume that is a second or two.

```sh
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
  -v /srv/backups:/backups backup-tower:latest snapshot my-container --stop always
```

```
my-container  2026-08-15T12-17-21Z
  stopped at /backups/my-container/2026-08-15T12-17-21Z
  volume   my-data                           3.7 MiB  (helper, 1334 files)
  total 3.7 MiB in 1.4s
```

**4. Check it is intact.** `verify` re-reads every archive and compares it against
the checksum recorded when it was written:

```sh
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
  -v /srv/backups:/backups backup-tower:latest verify my-container
```

**5. Unpack it yourself, without this tool.** This is the step that matters.
Look at what was written, then read one archive with plain `tar`:

```sh
sudo ls /srv/backups/my-container/
# 2026-08-15T12-17-21Z

sudo tar --zstd -tf /srv/backups/my-container/2026-08-15T12-17-21Z/volumes/my-data.tar.zst | head
```

If that lists your files, your backups are real. Everything else here is
convenience on top of it.

> Backups written from inside the container are owned by root with mode `0750`,
> which is why `sudo` appears above. Name the snapshot directory rather than
> using a `*` glob: your shell expands the glob before `sudo` takes effect, so it
> cannot see inside a root-owned directory.

## Running as a container

This is the normal way to run it. See [`compose.example.yaml`](compose.example.yaml)
for a complete, commented file; the essentials are:

```yaml
services:
  backup-tower:
    image: backup-tower:latest
    container_name: backup-tower
    command: ["daemon"]
    restart: unless-stopped
    environment:
      TOWER_BACKUP_DIR: /backups
      TOWER_INTERVAL: 6h
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /srv/backups:/backups
      - /opt:/opt:ro                 # your compose directories,
      - /etc/komodo:/etc/komodo:ro   # at IDENTICAL paths — see below
```

Two things about that file are not cosmetic.

**Mount your compose directories at the same path they have on the host.**
Compose creates containers on the host daemon but resolves relative paths against
*our* filesystem, so a compose file mounted anywhere else silently produces
containers with the wrong bind mounts. To list the directories you need:

```sh
docker ps -a --format '{{.Label "com.docker.compose.project.working_dir"}}' \
  | grep . | sort -u
```

**Helper containers.** To read another container's volumes, backup-tower starts a
short-lived helper from its own image. It works out which image that is by
itself; set `TOWER_HELPER_IMAGE` only if you renamed or retagged it.

### Configuration

| Variable | Default | Meaning |
|---|---|---|
| `TOWER_BACKUP_DIR` | `/backups` | Where snapshots go. A bare path means a local directory. |
| `TOWER_INTERVAL` | `6h` | How often the daemon checks for updates. |
| `TOWER_RETENTION_KEEP` | `3` | Snapshots to keep per container. |
| `TOWER_RETENTION_DAYS` | `14` | Keep anything younger than this, regardless of count. |
| `TOWER_ZSTD_LEVEL` | `3` | Compression level 1–19. During an update this is downtime. |
| `TOWER_CONCURRENCY` | `2` | Parallel snapshots. Disk throughput is the bottleneck. |
| `TOWER_HELPER_IMAGE` | auto-detected | Image for helper containers. |
| `TOWER_RULES_FILE` | — | Selection rules; most setups do not need one. |
| `KOMODO_URL`, `_API_KEY`, `_API_SECRET` | — | Komodo as a selection and credential source. |
| `KOMODO_TAG`, `KOMODO_SERVER` | — | Which tag opts in; which Komodo server this host is. |
| `DOCKER_HOST` | platform default | Engine endpoint. |

Logs go to the container's stderr — `docker logs backup-tower`. Add `-v` for
detail.

## Turning on automatic updates

**Nothing is updated unless you say so.** There is no built-in blocklist: which
containers are safe to update is your decision, not the tool's.

Label a container to opt it in:

```yaml
services:
  webapp:
    labels:
      tower.enable: "true"
      tower.snapshot.stop: "always"  # consistent snapshot, brief downtime
      tower.rollback: "true"         # undo automatically if it fails to start
```

### Labels

| Label | Default | Meaning |
|---|---|---|
| `tower.enable` | `false` | Opt in to automatic updates |
| `tower.monitor-only` | `false` | Report available updates, apply none |
| `tower.snapshot` | `true` | Snapshot before updating |
| `tower.snapshot.stop` | `auto` | `always` (consistent), `never` (no downtime), `auto` (stop only during updates) |
| `tower.snapshot.binds` | `false` | Include bind-mounted host paths |
| `tower.schedule` | — | Cron expression for backups independent of updates |
| `tower.retention.keep` / `.days` | `3` / `14` | Retention override |
| `tower.rollback` | `false` | Undo automatically when the health check fails |
| `tower.strategy` | `auto` | `auto`, `compose` or `recreate` |
| `tower.hook.pre-update` | — | Command run in the container before the update |
| `tower.hook.pre-snapshot` | — | Command run before the snapshot — a `pg_dump`, say |
| `tower.hook.post-update` | — | Command run once the replacement is healthy |

Only the `tower.*` namespace is read; other tools' labels are ignored on purpose.
A misspelled `tower.*` label is reported as a problem rather than silently doing
nothing.

### Then check, twice

```sh
backup-tower plan                  # who is selected, and why
backup-tower plan --explain webapp # the full reasoning for one container
backup-tower update --dry-run      # what would actually be updated right now
```

```
CONTAINER  ACTION   STRATEGY  SNAPSHOT      ENABLED BY
webapp     update   compose   always+binds  label: tower.enable
db         monitor  -         auto          rule: "watch the databases"

102 containers, 1 would be updated, 1 monitored only, 0 scheduled backups
```

`plan --all` lists everything, including what was *not* selected and why.

### Then let it run

```sh
backup-tower update              # once, now, everything that opted in
backup-tower daemon --once       # one full pass, then exit
backup-tower daemon              # keep going on the interval
```

`daemon` waits a full interval before its first pass, so starting it changes
nothing immediately. `--run-now` overrides that.

### What happens during an update

The image is pulled while the container is still serving, so the download is not
part of the downtime. Then the container is stopped once — for the snapshot and
the replacement together.

Containers that came from a compose file are updated with
`docker compose up -d --no-deps`, which is what you would do by hand. Everything
else is recreated through the API. If the compose plugin or the compose file is
not reachable, it falls back and says so rather than failing.

Recreated containers keep only your explicit settings, so the new image's own
entrypoint, environment and healthcheck apply.

**Afterwards it checks the container actually came up.** If the image declares a
`HEALTHCHECK`, that decides. If it does not, the container must simply stay up
for the settle window (15s), and the result says so plainly: *health verified only
by staying up*.

---

## Recovery

Something broke. Here is what to do.

### 1. Find the snapshot

```sh
backup-tower list webapp
```

```
CONTAINER  SNAPSHOT              AGE       TRIGGER  STATE    ARCHIVES  SIZE
webapp     2026-08-13T04-00-11Z  2d        update   stopped  1         3.7 MiB
webapp     2026-08-15T12-17-21Z  just now  update   stopped  1         3.7 MiB
```

`STATE` is `stopped` for a consistent copy, `hot` for a crash-consistent one.
`backup-tower show webapp <snapshot>` prints the full detail.

### 2. Roll back

```sh
backup-tower rollback webapp                       # to the latest snapshot
backup-tower rollback webapp 2026-08-13T04-00-11Z  # to a specific one
```

This restores the data, recreates the container from its captured configuration,
and puts it back on the image it was running at the time. It prints exactly what
it is about to replace and asks before doing it. In a script, pass `--yes`;
without a terminal it refuses rather than assuming.

To put back only the data and leave the container alone, use `restore` instead.

> **Restores replace, they do not merge.** Anything written since the snapshot is
> gone afterwards. That is the point, but check the snapshot's age first.

Archives are checksummed before anything is written. A snapshot that does not
match its manifest is refused — a half-restored volume is worse than an untouched
broken one. If you would rather have damaged data than none, `--skip-verify`
overrides that.

### 3. If the rollback also fails

The snapshot is still intact; nothing about a failed rollback damages it. The
original container is put back under its own name if the replacement could not be
created, so you are never left with no container at all.

To get at the data directly, without this tool:

```sh
sudo ls /srv/backups/webapp/                 # the snapshot directories
sudo ls /srv/backups/webapp/<snapshot>/volumes/   # the archives in one of them

mkdir /tmp/recovered
sudo tar --zstd -xf /srv/backups/webapp/<snapshot>/volumes/<name>.tar.zst -C /tmp/recovered
```

`spec.json` in the same directory is the container's full configuration as the
engine reported it.

### After a rollback

The container is left on a **digest reference** rather than a tag, and the next
check reports `pinned to a digest`. That is deliberate: the tag now points at the
release that just failed, so following it would immediately undo your rollback.
Re-point it at a tag once you have a fixed release.

---

## Scheduled backups

A container can be worth backing up nightly without ever being updated
automatically, so the two are separate. `tower.schedule` takes an ordinary cron
expression and needs nothing else enabled:

```yaml
labels:
  tower.schedule: "0 4 * * *"
```

Scheduled backups read **hot** by default — creating downtime on a timer is not a
decision the tool should make for you. Add `tower.snapshot.stop: "always"` if you
want a consistent copy and can accept the pause.

An invalid cron expression is reported by `plan` as a problem. A schedule that
never runs because of a typo is otherwise indistinguishable from one that was
never set.

## Retention and disk usage

A snapshot survives if it is among the most recent **N** *or* younger than **D**
days. Both apply: the count protects a rarely-updated container from ageing out
entirely, the age protects a busy one from losing last week.

```sh
backup-tower prune --dry-run   # what would go
backup-tower prune             # apply it
```

Retention also runs automatically after each scheduled backup and each update
pass.

**Snapshots you took by hand are never swept up automatically** — you took one
before making a change because you wanted it afterwards. `--include-manual`
overrides that.

Every update tags the image it replaced under `backup-tower/keep` so
`docker image prune` cannot remove it while a snapshot still needs it. `prune`
releases those tags along with the snapshots, including any left behind by a
snapshot directory that was deleted by other means.

## Selection in depth

Beyond labels, two other sources can select containers. Precedence is
**label > Komodo tag > rule file > default** — the more specific wins.

### Rule file

For containers whose compose file you would rather not touch. See
[`rules.example.yaml`](rules.example.yaml); point `TOWER_RULES_FILE` at your copy.
Rules apply in order and later matches override earlier ones, so write them
broad-first, specific-last. Unknown keys are rejected when the file is loaded.

### Komodo

If you run [Komodo](https://komo.do), it can drive the whole policy. Map tag names
to settings in the rule file, then tag a stack in the Komodo UI — no compose file
to edit, no redeploy:

```yaml
komodo_tags:
  - tag: bt-update
    set: {enable: true, monitor_only: false, retention_keep: 5}
  - tag: bt-snapshot-stop
    set: {stop: always}
  - tag: bt-rollback
    set: {rollback: true}
```

Tagged stacks resolve to local containers through their compose project name,
tagged deployments through their container name. Komodo manages several hosts
while backup-tower sees only its own, so set `KOMODO_SERVER` to this host's Komodo
server name — without it you get a warning rather than a guess.

Komodo also holds registry credentials for the images its stacks pull, and those
never reach the host's docker configuration. backup-tower reads them, which is
often the difference between a private image being checkable and not. Works with
Komodo 2.2 and newer.

If Komodo's auto-update is on for a stack you hand to backup-tower, turn it off.
Otherwise Komodo may update it first, without a snapshot, and the guarantee is
void.

### Registry credentials

Private images need credentials for the digest check and the pull. Two sources are
used, and both are tried in turn: this host's `docker login`
(`~/.docker/config.json`, or `DOCKER_CONFIG`), and Komodo's registry accounts.

### What cannot be updated

Containers on locally built images have no registry to check, and containers
referencing an image only by id have no name left to resolve. Both are reported as
such and skipped rather than failing on every run. Snapshots still work for them.

## Command reference

| Command | What it does |
|---|---|
| `info` | Which engine, which version, how many containers |
| `plan` | What would be acted on, and why. `--all`, `--explain <container>`, `--check` |
| `snapshot <container>` | Snapshot now. `--stop always\|never\|auto`, `--binds`, `--level N` |
| `list [container]` | Stored snapshots |
| `show <container> [snapshot]` | One snapshot in detail |
| `verify <container> [snapshot]` | Re-check archives against their checksums |
| `restore <container> [snapshot]` | Put data back. `--config`, `--image`, `--binds`, `--no-start`, `--skip-verify`, `--yes` |
| `rollback <container> [snapshot]` | Data, configuration and image together. `--yes` |
| `update [container...]` | Update. `--dry-run`, `--force`, `--settle`, `--health-timeout`, `--no-health-check` |
| `daemon` | Keep checking. `--once`, `--run-now`, `--interval` |
| `prune [container...]` | Apply retention. `--dry-run`, `--keep N`, `--days N`, `--include-manual`, `--yes` |

Global: `--backup-dir <path>`, `-v`/`--verbose`. Omitting `[snapshot]` means the
most recent one.

## Troubleshooting

**`cannot reach volume X directly and no helper image is configured`**
backup-tower could not work out its own image. Set `TOWER_HELPER_IMAGE` to the
image you built.

**`permission denied while trying to connect to the Docker daemon socket`**
Run as root, or add your user to the `docker` group. In a container, check the
socket is actually mounted.

**`the registry needs credentials`**
The image is private. Run `docker login <registry>` on the host, or let Komodo
supply the credentials.

**`the registry needs credentials that are stored in a credential helper`**
`credsStore` is set in `~/.docker/config.json` — common with Docker Desktop — and
this build cannot call external credential helpers. Use a config with plain
`auths` entries, or supply the credentials through Komodo.

**`skipped: pinned to a digest`**
Normal after a rollback, and deliberate. See [After a rollback](#after-a-rollback).

**`the compose plugin is not available here`**
It fell back to recreating the container through the API. Harmless, but a
compose-managed container is better updated by compose — use an image that has
the plugin.

**`update failed and was rolled back`**
Working as intended: the new release did not come up and the previous one is back.
`docker logs <container>` will say why it did not.

**`unknown label tower.xyz`**
A typo. Nothing was applied from that label.

**A snapshot takes much longer than the volume size suggests.**
Containers that ignore `SIGTERM` take the full stop timeout — 10 seconds by
default — before being killed, and for small volumes that dominates the downtime.

## Uninstall

```sh
docker rm -f backup-tower

# Release the images it was holding on to for rollbacks.
docker images 'backup-tower/keep' --format '{{.Repository}}:{{.Tag}}' \
  | xargs -r -n1 docker rmi

docker rmi backup-tower:latest
```

Some of those may report *"must force — container X is using its referenced
image"*. That is harmless: it means a container still runs on that image, so the
tag is all that would have gone anyway. Remove those with `docker rmi -f` once
nothing uses them.

Your backups in `TOWER_BACKUP_DIR` are plain files and are untouched by any of
this. Delete them yourself when you no longer want them, and remove the `tower.*`
labels from your compose files so nothing is left pointing at a tool that is no
longer there.

## Development

```sh
go test ./...                                        # unit tests
docker compose -f test/tower-test/compose.yaml up -d # throwaway stack
docker build -t backup-tower:dev .
```

The test stack covers the shapes that behave differently: a database in a named
volume, a bind-mounted host directory, a container with no storage at all, and
services with and without a `HEALTHCHECK`. Nothing publishes a port, so it cannot
collide with anything already running.

Never develop against production containers.

## Storage format

```
/backups/webapp/2026-08-13T14-22-05Z/
├── manifest.json     image digest, archive list, sizes, SHA256 per archive
├── spec.json         the untouched engine inspect response
├── volumes/pgdata.tar.zst
└── binds/_srv_webapp_data.tar.zst
```

Snapshot directories are named by UTC timestamp, so they sort chronologically.

## Not in scope

restic and remote backup targets, notifications, writable-layer snapshots, and the
Podman quadlet path. The events a notifier would need already run through one
place.

## Why it works this way

[DESIGN.md](DESIGN.md) explains why the tool behaves the way it does. Most of
those decisions were not planned in advance — they came out of a bug that had to
be fixed, and the explanation is there so the same mistake is not made twice.

## A note on how this was built

backup-tower was written with Claude (Anthropic) as a coding assistant. The
design decisions, the review, and the testing against a real host with a hundred
containers were done together; several of the bugs recorded in DESIGN.md were
found that way rather than by reading the code.
