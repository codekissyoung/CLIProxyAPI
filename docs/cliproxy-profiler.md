# CLIProxyAPI profiler watcher

`cliproxy-profiler` is a separate long-running helper binary for production diagnosis.
It watches `cliproxyapi` CPU usage and captures a compact evidence bundle only when configured thresholds are exceeded.

## Why this exists

`pprof` cannot replay a spike that already happened.
This watcher solves that by staying lightweight during normal operation and collecting evidence only when a spike reoccurs.

## What it captures

When a trigger fires, the watcher creates one timestamped directory under the configured output path and stores:

- `cpu.pb.gz` from `/debug/pprof/profile`
- optional `heap.pb.gz`
- optional `threadcreate.pb.gz`
- optional `goroutine.txt`
- `process.txt`
- `/proc/<pid>/status`, `limits`, `sched`, and `io`
- `ps` snapshots for the process and its threads
- `ss -s` summary and optional filtered `ss -tanp` details
- a tail of the request log when available
- a snapshot of the live `cliproxyapi.yaml`
- `metadata.yaml` describing the trigger and resolved runtime paths

## Build

```bash
go build -o /tmp/cliproxy-profiler ./cmd/cliproxy-profiler
```

For repeatable production upgrades on `glittering-book`, prefer:

```bash
./profiler-build.sh
```

The script:

- builds a versioned profiler binary under `~/deploy/bin`
- runs `-check` against the live `~/deploy/etc/cliproxy-profiler.yaml`
- installs the systemd unit from `examples/cliproxy-profiler/cliproxy-profiler.service`
- restarts `cliproxy-profiler`
- rolls back to the previous binary automatically if restart/startup fails

## Check current wiring before enabling the service

```bash
/tmp/cliproxy-profiler -config ./examples/cliproxy-profiler/cliproxy-profiler.example.yaml -check
```

`-check` verifies that:

- the watcher config parses correctly
- the target process can be found
- the runtime config resolves to a pprof URL
- the pprof index responds successfully

## Example config and systemd unit

Example files live here:

- `examples/cliproxy-profiler/cliproxy-profiler.example.yaml`
- `examples/cliproxy-profiler/cliproxy-profiler.service`

Recommended production flow:

1. Copy the example YAML to `/home/iec/deploy/etc/cliproxy-profiler.yaml`
2. Adjust the output directory and thresholds if needed
3. Run `./profiler-build.sh`

If you need a manual first-time install instead of the deploy script:

1. Copy the example YAML to `/home/iec/deploy/etc/cliproxy-profiler.yaml`
2. Adjust the output directory and thresholds if needed
3. Build and deploy the binary to `/home/iec/deploy/bin/cliproxy-profiler`
4. Copy the systemd unit to `/etc/systemd/system/cliproxy-profiler.service`
5. Run `systemctl daemon-reload && systemctl enable --now cliproxy-profiler`

## Operational notes

- The watcher is intentionally separate from the main `cliproxyapi` process.
- It does not modify live config files.
- Evidence capture adds some temporary overhead, especially while a CPU profile is being recorded.
- Keep `pprof.enable: true` and bind it to localhost when using this watcher.
- Retention settings should be kept conservative so long-running diagnosis does not grow without bound.
- The shipped example config uses conservative thresholds suitable for always-on production monitoring; tune downward only after observing real spike patterns.
