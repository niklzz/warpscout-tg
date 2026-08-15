# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

WARPSCOUT — a single-binary Go CLI that scans Cloudflare WARP endpoint pools, brings up a
userspace tunnel to each address (WireGuard / AmneziaWG / MASQUE), and reports where that
tunnel comes out (`SEEN AS` region, `NODE` colo), plus latency, loss and optional speed.
No root, no TUN device: everything runs on gvisor netstack inside the process.

## Commands

```sh
go build .                     # or: go run . scan
go test ./...                  # whole suite (no network needed; CI runs exactly this)
go test -run TestRenderConf     # single test
go test -run 'TestRenderConf$' -v
go vet ./...

# cross-compile like the release workflow does
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w -X main.version=1.2.3" .
```

Manual smoke run needs an account file first: `go run . register`, then `go run . scan -n 2`.

No Go toolchain is installed on this machine — run the above inside the same image the Dockerfile
uses (start OrbStack first, its daemon is usually down):

```sh
docker run --rm -v "$PWD":/src -w /src \
  -v "$HOME/.cache/warpscout-go":/root/.cache/go-build -v "$HOME/go/pkg/mod":/go/pkg/mod \
  golang:1.26-alpine sh -c 'go vet ./... && gofmt -l . && go test ./...'
```

## Layout

One flat `package main` at the repo root — no subpackages. Tests live in three files
(`warp_test.go` for almost everything, `tui_test.go`, `i1gen_test.go`); a new test for
`report.go` or `register.go` still goes into `warp_test.go`.

Platform splits are build-tagged pairs: [bind_linux.go](bind_linux.go)/[bind_other.go](bind_other.go)
(`SO_BINDTODEVICE` for `-interface`), [console_windows.go](console_windows.go)/[console_other.go](console_other.go)
(VT mode), [argv_android.go](argv_android.go)/[argv_other.go](argv_other.go) (Termux passes the
binary path as argv[1] via linker64), [dns_android.go](dns_android.go) (no `/etc/resolv.conf`).

## Architecture

**Commands** are table-driven in [cli.go](cli.go): each `command` carries its help text, its
`flagGroup`s (rendered into usage by [flags.go](flags.go)), a `setup` that registers flags into a
`flag.FlagSet`, and a `run`. Adding a command means appending to `commands` — nothing else dispatches.

**Flags → globals.** [flags.go](flags.go) fills an `options` struct, but a good deal of
configuration is written straight into package-level vars: `awgJc/awgJmin/awgJmax/awgI1`
([warp.go](warp.go)), `warpPorts`, `pools` ([pools.go](pools.go)), the account keys
(`warpPrivateKey`, `warpAddress…`, set by `applyAccount`), `masqueAcct`, `scanInterface`,
`outer` ([nest.go](nest.go)). `setupScan` in [main.go](main.go) is the one place that wires them
before any tunnel exists. This is deliberate (avoids threading state through `newTunnel`'s
callers) — **tests that touch these must save and restore them with `defer`**, as the existing
ones do.

**Tunnels** hide behind the `tunnel` interface in [tunnel.go](tunnel.go): `handshake`, `ports`,
`attempts`, `stack`, `Close`. Two implementations — `wgTunnel` (amneziawg-go device over
`netstack`, AWG params only differ in the UAPI string from `baseUAPI`) and `masqueTunnel`
([masque.go](masque.go), CONNECT-IP over QUIC or its HTTP/2 fallback, built on usque). Everything
above the interface — the scan loop, ping, meta fetch, speedtest — is protocol-agnostic. Tunnels
are expensive, so [workers.go](workers.go) creates one per worker and feeds jobs through a channel
(`runTunnelPool`); do not create a tunnel per endpoint.

**Scan flow** (`runScan` in [main.go](main.go)): phase 1 (`reachablePorts`,
[discovery.go](discovery.go)) finds which UDP ports the network lets out — primary ports first,
extended sweep only if none pass; skipped for MASQUE and for `-port N`. Phase 2 probes every
`probeTarget` through a tunnel and fetches `https://speed.cloudflare.com/meta`, which is where
region + colo come from ([meta.go](meta.go)). With `-tg` the same live tunnel then dials Telegram's MTProto DCs
([telegram.go](telegram.go)) — first DC to answer wins, and reaching Telegram outranks every latency
metric in `lessByLossRTT`. Results are `[]endpointResult` ([report.go](report.go)) → filters →
console tables / report file / generated config.

**The emitter seam.** All progress goes through `emitter func(tea.Msg)` ([tui.go](tui.go)).
`runWithUI` picks `p.Send` (bubbletea live dashboard) or `plainEmit` (line output) based on
`-plain`, `NO_COLOR` and whether stderr is a terminal. Scan code only emits messages
(`stepMsg`, `barBeginMsg`, `probedMsg`, `foundMsg`, `speedMsg`, `barEndMsg`, `doneMsg`) and never
prints directly — keep it that way, and add rendering to both sides when adding a message.

**Stream contract:** stderr carries progress, the TUI, notices and errors; stdout carries only
data (the tables, `-best`, `-conf -`). `version` must stay a bare version string on stdout —
`install.sh` compares its whole output.

**WARP-in-WARP** ([nest.go](nest.go)): `-through` dials an outer tunnel, then `tunnelBind`
implements amneziawg-go's `conn.Bind` on top of it so the scanned (inner) tunnels send their UDP
through the outer netstack. Under `-through`, `-proto` is the *outer* protocol and `-inner-proto`
is what is scanned; MTU drops by `nestedOverhead`, and host ICMP is replaced by `outer.pingTo`.

**Obfuscation search:** `find-junk` ([findjunk.go](findjunk.go)) and `find-sni`
([findsni.go](findsni.go)) rescan a small sample repeatedly with fresh AWG junk params / init
packet / MASQUE SNI until `-threshold` of endpoints come up, then print the `scan` command that
reuses the winning parameters. `-gen-i1` packets are built in [i1gen.go](i1gen.go), with the QUIC
Initial profile hand-rolled in [quic.go](quic.go).

**Accounts** ([register.go](register.go)) are persisted as `warpscout-account.json`; `register`
reuses an existing id+token to rotate keys unless `-fresh`. A separate MASQUE device is registered
alongside — `-p masque` fails without it. When the Cloudflare API is unreachable directly,
registration falls back to going through a WARP tunnel (or `-proxy`/`-relay`).

## Constraints worth knowing before "simplifying"

- `debug.SetMemoryLimit(32MiB)` in [cli.go](cli.go) and the batch-1 `conn.Bind` in
  [tunnel.go](tunnel.go) exist because gvisor + wireguard-go's default 128-packet batch OOM-kill
  the tool on small routers. Both are measured, not guessed.
- `-tun-ping-count` has a hard floor (`minDurabilityPings`): shorter bursts report DPI-torn
  endpoints as working.
- Most non-obvious constants and workarounds already carry a comment explaining the measurement
  behind them — read it before changing the number.

## Docs and release

- [README.md](README.md) and [README_RU.md](README_RU.md) are a translated pair: any user-visible
  change (flag, output, behaviour) must land in **both**.
- Commits follow Conventional Commits (`feat:`, `fix:`, `perf:`, `docs:`, `ci:`, `refactor:`),
  lowercase, imperative. `docs:` commits are filtered out of generated release notes.
- Pushing a `v*` tag triggers [release.yaml](.github/workflows/release.yaml) (six OS/arch archives,
  version injected via `-ldflags -X main.version`) and [docker-build.yaml](.github/workflows/docker-build.yaml).
  Annotated-tag body becomes the release notes header.
