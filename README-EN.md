<h1 align="center">WARPSCOUT-TG</h1>

<p align="center">A fork of <a href="https://github.com/vernette/warpscout">vernette/warpscout</a> that picks WARP endpoints Telegram actually works through.</p>

<p align="center">Documentation: <a href="README.md">🇷🇺 Русский</a> &middot; 🇬🇧 English</p>

## What this fork changes

Upstream scans Cloudflare WARP endpoints and reports where each one comes out - region, edge node, latency, loss - and ranks them by loss and ping. That ranking says nothing about whether Telegram is reachable from that exit, so `-best` and `-conf` can hand you a fast, well-placed endpoint through which Telegram does not connect.

This fork adds that check, and lets it decide:

- **`-tg`** - a `TG` column. As soon as a tunnel is up and verified, a real MTProto request (`req_pq`) is sent through that same tunnel to all five MTProto data centres at once, first DC to answer wins. A completed TCP connect alone proves nothing: DPI finishes the handshake toward Telegram and silently eats the payload, so `TG` only counts when a DC actually talked back. The column shows the time to its answer, or `blocked` when none did. No name is resolved and no SNI is sent - exactly what a Telegram client does, so the column is about the exit, not about some hostname surviving DPI. A blocked endpoint costs one `-timeout`, not five.
- **Reaching Telegram outranks every latency metric.** An endpoint that reached Telegram sorts above one that did not, whatever the ping says, so `-best` and `-conf` pick something that works.
- **`-tg-only`** - throw the rest away (implies `-tg`). Since only Telegram-working endpoints survive, they are ranked by the Telegram RTT instead of the endpoint ping - `-best` and `-conf` take the one Telegram is fastest through. If nothing reaches Telegram, the scan says so and exits non-zero instead of writing a report full of useless endpoints.

Nothing else is changed: same protocols, same flags, same output, same account files. Everything below is the short version - **the full documentation is upstream, in [vernette/warpscout](https://github.com/vernette/warpscout)**, and it applies here as written.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/niklzz/warpscout-tg/master/install.sh | sh

# OpenWrt routers have no curl by default
wget -qO- https://raw.githubusercontent.com/niklzz/warpscout-tg/master/install.sh | sh
```

Running it again is also how you update. Binaries for Linux, macOS, Windows and Android (`amd64`/`arm64`) are on the [Releases page](https://github.com/niklzz/warpscout-tg/releases); from source it is `go install github.com/niklzz/warpscout-tg@latest`.

## Commands

| Command     | What it does                                             |
| ----------- | -------------------------------------------------------- |
| `register`  | Create a WARP account and save it. Start with this.      |
| `scan`      | Scan endpoints and report the working ones.              |
| `find-junk` | Search for AmneziaWG settings that get through a filter. |
| `find-sni`  | Search for a MASQUE SNI that gets through a filter.      |
| `socks`     | Serve one endpoint as a local SOCKS5 proxy, to test it.  |
| `version`   | Print the installed version on a line of its own.        |

`warpscout <command> -h` lists every flag of any of them.

```sh
# 1. once - writes warpscout-account.json next to you
warpscout register

# 2. scan with the Telegram check
warpscout scan -p awg -tg

# only the endpoints Telegram works through, best one as ip:port
warpscout scan -p awg -tg-only -best

# ... and straight into an importable AmneziaWG config
warpscout scan -p awg -tg-only -conf warp.conf

# add in-tunnel RTT and loss, drop the Moscow node, keep German exits
warpscout scan -p awg -tg -P -exclude-node DME -country DE

# test one endpoint by hand through a local SOCKS5 proxy
warpscout socks -e "$(warpscout scan -p awg -tg-only -best)" -p awg
```

`-p awg` is the one to reach for on a filtered network; if it finds nothing, `-gen-i1 quic` is the next thing to try, and `find-junk` after that. Docker, WARP-in-WARP (`-through`), MASQUE, obfuscation and troubleshooting all work as upstream documents them.

## License

MIT, as upstream. Credits, screenshots and the donation links belong in [the original repository](https://github.com/vernette/warpscout) - go there.
