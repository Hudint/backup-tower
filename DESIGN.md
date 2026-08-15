# Design notes

Why backup-tower works the way it does. None of this is needed to use the tool —
see the [README](README.md) for that.

Most of what follows was not designed up front. Each section describes a bug that
had to be fixed and the rule that came out of fixing it, written down so the
reasoning survives longer than the memory of the incident.

## The central bet

During an update the container is stopped anyway. That is the one moment when
its volumes are cold and a consistent copy costs nothing — no `pg_dump`, no
`fsfreeze`, no knowledge of what the application is. Every other design choice
follows from wanting that moment and nothing else.

It also decides the sequence: pull first, while the container is still serving,
so the download is never part of the downtime. Then stop once, for the snapshot
and the replacement together.

## Storage

Archives are plain tar compressed with zstd, and nothing else. If backup-tower is
gone, broken, or a version that cannot read its own older output, `tar --zstd -xf`
still gets the data out. That property is worth more than deduplication,
encryption, or a clever container format, and a test extracts an archive with the
system `tar` binary so it cannot quietly stop being true.

zstd at level 3 rather than gzip: during an update, compression time *is*
downtime. Level 3 buys most of the ratio at a fraction of the cost.

The root directory of an archive is stored as `./`, the same convention
`tar -C dir .` uses. Without it the directory's own mode is lost — and PostgreSQL
refuses to start when its data directory is group-readable, so a restore would
have looked complete and not run.

## Reaching volumes

A container cannot add mounts after it has started, so backup-tower running in a
container cannot mount another container's volumes into itself. It starts a
short-lived helper container instead, using its own image, because Go does tar
and zstd natively — no Alpine, no shell pipes, no external binaries, and one
implementation rather than two that can drift.

The helper image is worked out from the container's own hostname, falling back to
the container id in `/proc/self/mountinfo`. The mount table is consulted because a
custom hostname defeats the first method, and because under cgroup v2
`/proc/self/cgroup` no longer carries the id. Only the engine's own storage paths
are searched: a bind-mounted content-addressed cache would otherwise offer up a
64-character hex name of its own.

When the volume path is directly readable — running on the engine host with
sufficient rights — that faster path is used and no helper is started.

## Recreating a container

The engine's inspect response mixes configuration with runtime state, and handing
the state back is how recreated containers end up subtly wrong: a hostname frozen
to a dead container's id, DNS aliases pointing at an id that no longer exists, IP
addresses the engine has since given to someone else. Only the requested parts
are replayed — static addresses, aliases, links, driver options — and the
assigned parts are dropped.

**The subtlety that decides whether updates are correct at all.** That same
inspect response does not distinguish what the operator asked for from what the
image supplied. `CMD`, `ENTRYPOINT`, `HEALTHCHECK`, `ENV` and `LABEL` all look
like they were requested. Recreating a container with the whole set pins the new
release to the *old* image's defaults: a release that changes its entrypoint runs
with the previous one, and — worst of all — a broken release passes the health
gate, because the healthcheck being evaluated is the previous version's. This was
found by pushing a deliberately broken image and watching it be declared healthy.
backup-tower subtracts the old image's own defaults and keeps only what differs.

The outgoing container is renamed aside rather than deleted, and only removed once
its replacement exists. If creation fails, the original is put back under its own
name. Being left with no container at all would be a worse failure than the update
that was being attempted.

## Deciding whether an update exists

The comparison is on manifest digests, not tags. A moving tag like `:latest` says
nothing about whether the content changed, and pulling every image just to find
out is expensive enough on a busy host that people turn updates off. The engine's
own distribution endpoint answers the question without downloading anything.

Registry credentials come from two sources — this host's `docker login` and
Komodo's registry accounts — and every candidate is tried in turn rather than
ranked. Credentials are per-registry, not per-container, so any precedence rule
would be a guess; asking twice is cheaper than being wrong.

## The health gate

A container that is merely running proves very little: a broken application can
sit in a restart loop for a while, or start and fail on the first request. When
the image declares a healthcheck, that is by far the better signal.

Whether a healthcheck exists is read from the configuration, not from whether the
engine has populated the health state yet. Right after a start it has not, and
treating that moment as "no healthcheck" let the weaker runtime check decide a
case the healthcheck was there to judge — which is exactly how a broken release
once passed.

Without a healthcheck the honest fallback is to watch for a crash loop over a
settle window, and to say so in the result rather than claim more than was
checked.

## Keeping the way back open

After an update the previous image loses its tag, which makes it fair game for
`docker image prune`. Every snapshot tags it under `backup-tower/keep` so it
survives exactly as long as the snapshot does, and pruning the snapshot releases
the pin. Without the first half the rollback path works right up until someone
tidies up; without the second half the image store grows by one tagged image per
update, forever, and `docker image prune` cannot help because every one of them
is tagged.

A rollback leaves the container on a digest reference rather than a tag. The tag
now points at the release that just failed, so resolving it would immediately undo
the rollback.

## Scheduling without a state file

When a scheduled backup last ran is read back from the backup store, which already
records when each snapshot was taken and why. A restarted daemon therefore neither
repeats a backup it just took nor skips one it owes, and there is no second source
of truth to drift out of step with the first.

A container with no history is not retroactively owed anything: the first fire
time is counted from when the daemon started.

## Choosing what to act on

Automatic updates are opt-in, and there is no built-in blocklist. Which containers
are safe to update is the operator's decision, not the tool's — several stacks
ship their own updaters, and a tool that quietly overrode that would be worse than
one that merely mentions it.

Only the `tower.*` label namespace is read. Acting on another tool's labels —
Watchtower's in particular — would mean touching containers that were marked for
something else entirely.

A misspelled `tower.*` label is reported as a problem rather than ignored. A typo
in the label that was meant to enable or protect a container otherwise does
nothing and says nothing, which is the worst way to find out.

Komodo is used as a selection source only; updates still go through the normal
path. That keeps the coupling to a handful of read calls rather than tying
backup-tower to Komodo's API version and its idea of when a deployment is
finished. Its request names are tried under both their pre- and post-2.3.0
spellings, because pinning either one means silently losing credentials on one
side of that boundary.

## Reporting

A strategy that hands back the same container did not replace anything, and
saying "updated" would be a false statement. Compose reaches that conclusion
whenever the image and the configuration it computes are both unchanged.

Warnings are printed on stdout with the report they belong to. On stderr they
interleave with it and end up appearing under the wrong container — which
produced "container has no storage to archive" under a database that had just
archived a 3.7 MiB volume.

A tool like this is only useful if its output can be trusted, so a report that
overstates what happened is treated as a defect rather than a cosmetic issue.
