# Microtronix ZFS backup storage — bench and fit (2026-10-13)

A 1 TB "Premium ZFS Storage" account at Microtronix ESolutions (Ottoville, Ohio, their own
AS400175), `bak3.microtronix-tech.com`, bought 2026-10-13. Access is a jailed GNU `rush`
shell that permits only `scp`, `sftp`, `rsync` (and `rdiff`); every other command — even
`ls` — answers "You are not permitted to execute this command". OpenSSH 8.9 (Ubuntu 22.04),
ZFS home (`/zpool5/<user>`), 1.0 TB quota, 2^31 inodes. Key auth works (a Mac key and the SLC
box's key were installed through `rsync` of `.ssh/`; the e-mailed password still works and
should be rotated in the client portal). Measured from the prod box (SLC, 10 Gbps) and from a
Mac in El Paso (42 MB/s uplink to Cloudflare, 19–26 MB/s single-stream downlink). Every
transfer md5-verified. Bench files deleted afterwards; the script is kept at
`/var/tmp/slcbench.sh` on SLC for a re-run at another hour.

## Throughput (SLC → bak3, RTT 50 ms via Hurricane Electric, 2026-10-13 17:54–18:07 UTC)

| Transfer | Result |
|---|---|
| scp up 256 MiB, chacha20 / aes128-gcm | 5.3 / 5.1 MB/s |
| scp up 1 GiB | 9.2 MB/s |
| rsync up 256 MiB (`-W`) | 5.9 MB/s |
| scp up 4 × 256 MiB parallel | 12.5 MB/s aggregate |
| scp up 8 × 256 MiB parallel | 16.2 MB/s aggregate |
| scp / sftp down 256 MiB | 14.1 / 13.6 MB/s |
| scp down 1 GiB | 13.1 MB/s |
| scp down 4 × 256 MiB parallel | 24.9 MB/s aggregate |
| scp down 8 × 256 MiB parallel | 36.6 MB/s aggregate |
| rsync 500 × 8 KiB files | 3.9 s = 128 files/s; no-change re-run 1.9 s |
| sftp, 20 sequential 8 KiB puts in one session | 327 ms per file |

From the Mac (RTT 56 ms): up 1.3–1.5 MB/s single, 3.4 MB/s with four streams; down 9–11 MB/s
single, 20.7 MB/s with four.

The terms promise a 1 Gbps port and the marketing "10G backup nodes"; what the account
delivers is 5–9 MB/s per stream and ~16 MB/s total inbound from a 10 Gbps box. Filling the
terabyte would take ~18 h, reading it back ~8 h. Whether this is per-account shaping, a busy
node or the HE.net path is not known; one run only, re-measure before relying on it.

The 1 MiB-change rsync delta test did not transfer (a test flaw — size+mtime quick check);
not measured.

## Speed diagnosis (2026-10-13 18:34–20:12 UTC, from SLC)

The limit is the path into Microtronix, not anything on our side or in SSH.

| Lever tried | Effect |
|---|---|
| Cipher (chacha20 / aes128-gcm / aes128-ctr+umac64) | none (5–9 MB/s up, all within noise) |
| `IPQoS=none` / `throughput` (scp marks bulk traffic CS1 "lower effort") | none (11.6 / 8.0 vs 9.2 MB/s baseline) |
| SFTP pipelining `-B 261120 -R 64` and `-R 128` (16 / 32 MiB outstanding) | none (7.5 / 7.4 MB/s). `-B 262144` and above is refused by the server: exit 255, a 0-byte file — the "55 MB/s" of the first pass was that failure |
| rclone `--sftp-chunk-size 255k --sftp-concurrency 128` | none (7.9 vs 7.7 MB/s) |
| `--sockopts=SO_SNDBUF=16M` | none |
| Parallel streams, upload (8 / 16 / 32 × SFTP) | 16.2 / 14.0 / 13.5 MB/s — a flat ceiling of ~110–130 Mbps |
| Parallel streams, download (4 / 8 / 16 / 32) | 24.9 / 36.6 / 33.7 / 25.5 MB/s — peaks at 8 |

What the sending socket said during a 256 MiB scp upload (`ss -ti`, every 3 s): never
`rwnd_limited`, never `sndbuf_limited`; the peer's advertised window grows to 3 MiB (their
Linux default `tcp_rmem`) and ours is 64 MiB with BBR; cwnd reaches 7,065 segments while only
~1,400 are in flight. Then the path itself: RTT swings 48 → 130 ms as soon as we push, one
burst of 330 lost segments at +9 s collapsed cwnd to 4, and 6,421 segments were retransmitted
in 28 s — 275 MB sent for 268 MB of file, a 2.6 % retransmit rate on a link that shows 0 %
ping loss when idle. That is a shaper or a saturated link on their ingress (or the Hurricane
Electric ↔ Microtronix hand-off: the Mac's path from AT&T ends on the same two Microtronix
hops and was slower still). BBR settles at the ~70 Mbps the path delivers per stream; eight
streams share ~130 Mbps. Download has no such loss and is bounded by their send side at
~13–15 MB/s per stream, ~37 MB/s total.

Two more findings for anyone scripting against it:

- **More than ~10 simultaneous SSH connections are dropped** (the sshd `MaxStartups`
  default): of 16 parallel SFTP downloads only 10 verified, of 32 only 18 — the rest never
  connected. Keep concurrency at 8 or stagger connection opening.
- **rclone v1.60.1 hung for 74 min after a complete download** (the file was whole and its
  md5 right; the process never exited and held no connection). Any use must carry
  `--timeout`, as `pg-backup.sh` already does, and the box's rclone should be upgraded.

What is left to try: a re-run off their business hours (`/var/tmp/diag3.sh` on SLC prints
the socket samples; 20:00 UTC was 16:00 in Ohio), and a ticket asking Microtronix whether
`bak3` carries a per-account shaper or is saturated, with these numbers — a move to another
node is the only fix that is theirs to make. No IPv6 on either side, FTP on the box is
plaintext only (no AUTH TLS), nothing else listens.

### Controls: is it our end? (20:42–20:59 UTC)

No. Measured from the same box within the same half hour:

| Control | Result |
|---|---|
| SLC NIC | 10 Gbps full duplex, 0 TX errors / drops; `fq` qdisc, 82 drops in 243 GB sent; path MTU 1500; BBR, 64 MiB buffers, window scaling / SACK / timestamps on |
| iperf3 → Dallas (RTT 40 ms) | 93 MB/s one stream (7 retransmits), 691 MB/s with 8 |
| iperf3 → Ashburn (RTT 48 ms) | 66 MB/s one stream (5 retransmits), 526 MB/s with 8; 65 MB/s down |
| iperf3 → New York (RTT 50 ms) | 63 MB/s one stream (3 retransmits), 511 MB/s with 8; 67 MB/s down |
| iperf3 → Washington DC, Leaseweb (RTT 52 ms) | 307 MB/s one stream, 695 MB/s with 8 |
| HTTPS upload → Cloudflare, 277 MB | 50–76 MB/s one stream, 18 retransmits |
| scp → Microtronix (RTT 48–130 ms) | 5–9 MB/s one stream, ~4,500–6,400 retransmits per 256 MiB |

Same box, same hour, same distance: every other destination takes 60–300 MB/s per stream
with a handful of retransmits; Microtronix takes 5–9 MB/s with thousands. Congestion control
was also checked on the path itself through a proxy socket that sets `TCP_CONGESTION`: BBR
3.7–4.2 MB/s against cubic 0.7–0.9 MB/s on back-to-back runs — the lossy path punishes
cubic, so the box's BBR is already the right choice. The box-wide retransmit rate since
boot is 1.0 % (every customer's last mile counts in it), so it says nothing by itself. Two
intended controls failed: `iperf.he.net` resets or times out, and `mirrors.he.net` answers
404 to a listing — so the Hurricane Electric leg specifically was not isolated; the
Microtronix side of the HE hand-off or Microtronix's own edge are the candidates. iperf3 was
taken from Ubuntu's signed package and extracted without root; nothing was installed.

## Price and terms

- $2.00 per TB per month, no ingress or egress fees, no contract (the product page and the
  provider's LowEndTalk threads). 1 TB on this account.
- Backup Terms of Service: the space is "solely to store backups"; file sharing is banned;
  the service is "AS IS" with liability for data loss excluded; terms change on posting;
  termination without notice on a violation. "The only limitation in place is the 1GBps port."
- SLA: 99 % monthly server uptime, pro-rated credits from 98.9 % down; routing and upstream
  issues excluded. The "100 % no data loss" guarantee applies only to the separate Geo-Sync
  packages (nightly rsync to another datacenter), not to this one.
- Snapshots: 7 daily ZFS snapshots kept on the provider side (the product page; the terms
  promise nothing). Pools: RAID-Z1 up to 20 TB, Z2 above (a staff post).
- Outage record (their own announcements): a two-week unstable fibre splice (Dec 2024), a
  30 min BGP event (2026-02-11), a ~6.5 h layer-2 incident (undated). One datacenter.

## Where it fits

| Use | Verdict | Why |
|---|---|---|
| Second off-box leg of the daily Postgres dump + Prometheus rules (`deploy/scripts/pg-backup.sh`) | **Yes, now** | 33 MB/day lands in seconds even at 5 MB/s; 30 days × 33 MB ≈ 1 GB of the terabyte. It is a different provider, a different account and a different protocol from Sync.com, and the provider-side ZFS snapshots are the one copy our own credentials cannot delete — the Sync copy can be wiped by whoever holds `sync-backup.env`. rclone's `sftp` backend makes it a second `OFFBOX_REMOTE` plus one metric (`vaultaire_backup_offbox2_last_success_timestamp_seconds`) and one rule. Squarely "backups" under their terms |
| Age-encrypted copy of `/opt/vaultaire/configs` (the `.env`) | Possible | The script keeps configs on the box only; an encrypted copy here would survive the box. Only ever encrypted — the password arrived in plain e-mail and the provider disclaims everything |
| Vault parity leg (`VAULT_PARITY_LEGS`) | **No** | 16 MB/s in / 37 MB/s out with 8 streams against Sync's 156 / 450 and Lyve's 240 MB/s rebuild; a parity rebuild of one 1 GiB object would take ~75 s. Needs a new SFTP driver (`pkg/sftp` is not a dependency). Customer bytes on a "backups only, AS IS, 99 %" account is a terms and durability question the price does not answer |
| Data-plane backend (Standard, Vault, pack store, CDN origin) | **No** | No S3 or WebDAV, 327 ms per small file, the throughput above, one site, no durability warranty |
| Cold third copy of OUR data (tenant zero, bench archives, Geyser admin exports) | Yes, as capacity allows | It is a backup; 1 TB at $2 is the cheapest shelf we have; fill rate ~16 MB/s |

## Access

Host, user and password are in the Keychain item `microtronix-bak3` (never in this repo or
`.env`). Keys: `~/.ssh/id_ed25519_microtronix` (Mac), `~/.ssh/id_ed25519` of `user1` on SLC.
SFTP `df -h .` is the only way to read the quota; `rsync --list-only` the only directory
listing besides sftp.
