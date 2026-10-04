# Kontakt
[Читать на русском](README.md)

## Run and update

### Once: what to install

Everything runs in a Linux shell: **Linux**, **WSL on Windows** or **Termux on Android**.
On Windows run `wsl` in PowerShell first: commands like `TUNNEL=1 command` are bash syntax.

```bash
# Ubuntu / WSL: Go 1.24+ from go.dev (apt is often too old)
curl -LO https://go.dev/dl/go1.24.7.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.24.7.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin' >> ~/.bashrc && source ~/.bashrc
# cloudflared for a public link (the Linux build; it does not touch Windows tunnels)
mkdir -p ~/.local/bin && curl -L https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 -o ~/.local/bin/cloudflared && chmod +x ~/.local/bin/cloudflared

# Termux
pkg install golang git cloudflared curl

# the project
git clone https://github.com/Dmitry-Fedotov-Dev/kontakt && cd kontakt
```

### Run

One terminal window per command (Termux: swipe from the left edge → NEW SESSION). `Ctrl+C`
stops only this script's processes; other tunnels and services are left alone, free ports are
picked automatically.

| What | Command | Open |
|---|---|---|
| **Kontakt** with a public link | `./scripts/cluster-tunnel.sh` | `https://….trycloudflare.com` from the output; radio — same link + `/radio/` |
| Kontakt on this machine only | `./scripts/run.sh` | `http://localhost:8080`, radio — `http://localhost:8080/radio/` |
| **Open Radio** with a public link | `TUNNEL=1 bash scripts/radio.sh` | link in the output; locally `http://localhost:27620` |
| Radio on this machine only | `bash scripts/radio.sh` | `http://localhost:27620` |
| Mesh demo (Master + 5 nodes) | `./scripts/mesh-demo.sh` | graph in Grafana |
| **With monitoring** | prefix the Kontakt or radio command with `MONITORING=1` (only one of them) | Grafana address in the output (usually `http://localhost:27630`) |
| Monitoring alone | `monitoring/stack.sh up` / `down` | `http://localhost:3002` |

- The link is printed only once the tunnel is connected to Cloudflare (5–20 s).
- Browsers grant the microphone only over https or on `localhost`: other devices use the tunnel link.
- To test a call use two different browsers (or a normal + an incognito window).
- Termux: run `termux-wake-lock` first and start scripts with `bash scripts/…`.
- Monitoring picks its mode: Docker on Linux, a separate compose file for Docker Desktop, or
  plain binaries without Docker (`MONITORING=local`). Empty graphs — check
  `http://localhost:27631/targets`: targets must be UP.

### Update

```bash
git pull
```

| What | How to apply | Tunnel link |
|---|---|---|
| Kontakt | `Ctrl+C`, start again | **changes** |
| Radio | in a **second** window: `bash scripts/radio.sh update` (pulls and builds; on a build error the old version keeps running) | **stays**; host and listeners reconnect by themselves |
| Grafana dashboards | nothing — picked up in ~10 s | — |
| Prometheus config, alert rules | `monitoring/stack.sh down && monitoring/stack.sh up` | — |

Open pages keep the old version until the tab is reloaded. A permanent link on your own
domain: `./scripts/tunnel.sh publish <domain>`. Test plan: [docs/TEST_CASES.md](docs/TEST_CASES.md) (Russian).

---

<p align="center">
  <img width="427" height="900" alt="Kontakt Screenshot" src="https://github.com/user-attachments/assets/ee221aa9-4ef8-4bf3-8cf6-3c27cf632ed8" />
</p>

Pick up the phone and start talking to a random stranger.

## Key Features

- **Browser Protocol:** Runs directly in the browser via WebSocket ([RFC 7118](https://datatracker.ietf.org/doc/html/rfc7118)). Connect seamlessly from any browser-enabled device.
- **Microservice Architecture:** Powered by three Go services: **web**, **signal**, and **media** (managed via gRPC).
- **Privacy & Security:** Voice data is never stored anywhere. Moderation is shared with the radio (one cookie, one database): a report gives a yellow card, reports from two different people within a week ban the cookie for good — per zone: calls, going on air, or both. Reports from brand-new cookies split the pair but do not count. Hardware-based bans are planned.
- **High Efficiency:** 100 channels (50 simultaneous calls) consume as little as 0.22 CPU cores and 43 MB RAM. See [LOAD_REPORT.md](LOAD_REPORT.md) for benchmark details.
- **Bandwidth:** a channel costs about 69 kbps per direction (G.711 + RTP, measured from `kontakt_media_bytes_*_total`; about 80 kbps with IP/UDP headers). Capacity is **measured, not assumed**: Cloudflare Tunnel has no official Mbps limit — it depends on traffic, sessions and the host — so it comes from load tests (`k6/load.js`) and metrics. Quick Tunnel (`*.trycloudflare.com`) is for development and demos only: no uptime guarantee, 200 in-flight HTTP requests, a temporary hostname.

---

## Open Radio (prototype)

A separate program on the same base: anyone can take a free frequency (87.5–108.0) and
broadcast, anyone can turn the dial and listen. The host queues mp3 files and switches the
microphone on (music ducks under the voice); between stations the listener hears static,
beat whistles and crackle.

```bash
./scripts/radio.sh            # http://localhost:27620 (or the next free port)
TUNNEL=1 ./scripts/radio.sh   # plus a temporary https://….trycloudflare.com address
```

The host's browser encodes G.711 μ-law (8 kHz, 64 kbit/s); the server only relays ready
160-byte frames to listeners of that frequency — no decoding or mixing. mp3 files are never
uploaded. Limits: `-max-stations`, `-max-listeners`; host stream capped at 64 kbit/s.
