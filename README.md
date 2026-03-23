# rns-iface-email

Point-to-point email transport bridge for [Reticulum](https://reticulum.network/) mesh networking. Runs as a PipeInterface subprocess of `rnsd`, forwarding RNS packets over IMAP/SMTP email to exactly one configured remote peer.

![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)
![Go 1.26+](https://img.shields.io/badge/go-1.26%2B-00ADD8.svg)
![Platforms: linux/darwin/windows · amd64/arm64](https://img.shields.io/badge/platforms-linux%20%7C%20darwin%20%7C%20windows-lightgrey.svg)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/x3ps/rns-over-email)

---

## Table of Contents

1. [Architecture](#architecture)
2. [Installation](#installation)
3. [rnsd Integration](#rnsd-integration)
4. [Configuration Reference](#configuration-reference)
5. [Build & Test](#build--test)
6. [Dependencies](#dependencies)
7. [License](#license)

---

## Architecture

```
rnsd <-stdin/stdout-> rns-over-email <-SMTP/IMAP-> mail server <-> remote peer
```

The process communicates with rnsd via HDLC-framed stdin/stdout using [go-rns-pipe](https://github.com/x3ps/go-rns-pipe). Each process bridges a single remote peer.

### Delivery semantics

This bridge operates as a **lightweight best-effort transport**:

- **Outbound (RNS->email)**: Packets consumed from pipe stdin are encoded as MIME and sent via SMTP with 3 attempts (1s/2s exponential backoff). If all retries fail, the packet is **lost at this layer** and a recovery probe loop begins with configurable exponential backoff (`--smtp-recovery-delay` / `--smtp-max-recovery-delay`). During recovery, the interface is signalled offline; once a probe succeeds, it is signalled back online. If the SMTP DATA dot-terminator is sent but the server's final response is lost (timeout/EOF), the outcome is ambiguous — the handler does **not** retry (at-most-once preserved) and triggers recovery for the SMTP path. Higher-layer RNS protocols (Link/Resource) may detect the loss via their own ACK/timeout mechanisms, but basic `Packet` sends are at-most-once.
- **Inbound (email->RNS)**: An IMAP worker (IDLE with poll fallback) fetches emails, decodes packets, and injects them into rnsd via `iface.Receive()`. Checkpointed by UID. RNS deduplicates if the same packet arrives twice. Decode failures preserve messages for retry (no data loss).
- **SMTP auth**: PLAIN is preferred when the server advertises it. If only LOGIN is available, LOGIN is used as a fallback. If neither is advertised, PLAIN is attempted as a compatibility last-resort.

### Inbound processing

Inbound messages are classified into three categories:

1. **Not ours** (skipped): non-transport mail (wrong Content-Type, no transport markers) or transport mail from a non-peer sender / to a wrong recipient. Skipped messages do not block the checkpoint, are not deleted/moved by cleanup, and remain in the mailbox.
2. **Ours but broken** (preserved): transport mail that matches transport signals but fails to decode (corrupt base64, unparseable From/To headers). Preserved for retry; blocks the checkpoint to prevent data loss.
3. **Ours and valid** (processed): transport mail from the correct peer, to the correct local address, successfully decoded and injected into RNS.

Transport envelope identification: `X-RNS-Transport: 1` header present AND `Content-Type: application/octet-stream`.

From/To address validation applies to all transport messages. The `From` header must match `--peer-email` and the `To` header must match `--smtp-from` (both normalized to bare addresses).

**Note**: Sender/recipient validation is protocol hardening (From/To header match), not cryptographic authentication — email headers can be spoofed by anyone with access to the mail server.

**Duplicate injection**: When a corrupt transport message blocks the checkpoint, valid messages above it are still injected into RNS but not checkpointed. On session restart, they will be re-fetched and re-injected. This is safe because RNS deduplicates packets by hash.

### Transport email format

Outbound transport messages are sent as a single-part MIME email with a raw RNS packet in the body:

```eml
From: sender@example.com
To: peer@example.com
Date: Tue, 23 Mar 2026 12:34:56 +0000
Message-ID: <550e8400-e29b-41d4-a716-446655440000@example.com>
MIME-Version: 1.0
Content-Type: application/octet-stream
Content-Transfer-Encoding: base64
X-RNS-Transport: 1

AQIDBAUGBwgJCgsMDQ4PEA==
```

Header semantics:

- `From` — local transport email address (`--smtp-from`).
- `To` — remote peer email address (`--peer-email`).
- `Date` — UTC timestamp of envelope creation.
- `Message-ID` — unique identifier generated for the email; useful for diagnostics and mail server tracing.
- `MIME-Version: 1.0` — declares MIME formatting.
- `Content-Type: application/octet-stream` — marks the body as opaque binary transport payload rather than human-readable text.
- `Content-Transfer-Encoding: base64` — encodes the raw RNS packet into RFC-compliant mail-safe text.
- `X-RNS-Transport: 1` — transport marker identifying this as an RNS transport envelope.

Body semantics:

- The body is the raw RNS packet bytes encoded with Base64.
- Base64 lines are wrapped at 76 characters per RFC 2045.

### Config validation

Invalid values in environment variables cause immediate startup failure rather than silently falling back to defaults. Integer-only variables (`RNS_EMAIL_PIPE_MTU`, `RNS_EMAIL_SMTP_PORT`, `RNS_EMAIL_IMAP_PORT`, `RNS_EMAIL_SMTP_RECOVERY_DELAY`, `RNS_EMAIL_SMTP_MAX_RECOVERY_DELAY`, `RNS_EMAIL_IMAP_RECONNECT_DELAY`, `RNS_EMAIL_IMAP_MAX_RECONNECT_DELAY`) reject non-integer input. Duration variables (`RNS_EMAIL_IMAP_POLL_INTERVAL`) reject unparseable Go duration strings. `max_*` delay values must be greater than or equal to their corresponding base values.

### State

- **checkpoint.json** — IMAP polling watermark (folder + uidvalidity -> last_uid). Atomic writes (temp+rename).

Example:

```json
{
  "folder": "INBOX",
  "uidvalidity": 12345,
  "last_uid": 678
}
```

Field semantics:

- `folder` — IMAP mailbox name this checkpoint belongs to, typically `INBOX`.
- `uidvalidity` — IMAP `UIDVALIDITY` value for that mailbox. This distinguishes one mailbox identity/version from another.
- `last_uid` — highest successfully checkpointed IMAP UID processed for that `folder` and `uidvalidity`.

State behavior:

- The file tracks only inbound IMAP progress. It does not store outbound queue state, packet history, or peer metadata.
- If `uidvalidity` changes, the old checkpoint is treated as invalid and progress restarts from UID `0` for the new mailbox identity.
- If the file is missing, the worker starts from UID `0`.
- If the file is corrupt JSON, the worker logs a warning and starts from UID `0`.
- Writes are atomic: the file is written to a temporary path and then renamed into place.
- The current implementation stores a single checkpoint record, not a map of multiple folders.

---

## Installation

### go install

```sh
go install github.com/x3ps/rns-iface-email/cmd/rns-over-email@latest
```

### Build from source

```sh
git clone https://github.com/x3ps/rns-iface-email.git
cd rns-iface-email
go build ./cmd/rns-over-email
```

### Pre-built binaries

Download the latest release archive for your platform from the [Releases](https://github.com/x3ps/rns-iface-email/releases) page. Available targets: `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`.

---

## rnsd Integration

Add a `PipeInterface` block to your Reticulum config:

```
[[interfaces]]
  type = PipeInterface
  name = EmailTransport
  command = /path/to/rns-over-email --smtp-host smtp.example.com --smtp-username user --smtp-password-file /run/secrets/pw --smtp-from user@example.com --imap-host imap.example.com --imap-username user --imap-password-file /run/secrets/pw --peer-email peer@example.com
  respawn_delay = 5
```

Each interface block connects to exactly one peer. To bridge multiple peers, add one block per peer with a distinct `name` and `--peer-email`. The checkpoint file is automatically derived from the pipe name (e.g. `checkpoint-EmailTransport-<hash>.json`), so multiple instances sharing a working directory won't overwrite each other's state. If you need explicit control, use `--checkpoint-path`.

---

## Configuration Reference

All configuration is via CLI flags and/or environment variables (`RNS_EMAIL_*`).

Precedence: defaults → env → flags → password-files.

### Quick start

```sh
./rns-over-email \
  --smtp-host smtp.example.com --smtp-port 587 \
  --smtp-username user@example.com --smtp-password-file /run/secrets/smtp-pw \
  --smtp-from user@example.com \
  --imap-host imap.example.com --imap-port 993 \
  --imap-username user@example.com --imap-password-file /run/secrets/imap-pw \
  --peer-email peer@example.com
```

### Pipe

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--pipe-name` | `RNS_EMAIL_PIPE_NAME` | `EmailTransport` | RNS pipe interface name |
| `--pipe-mtu` | `RNS_EMAIL_PIPE_MTU` | `500` | Pipe MTU in bytes |

### SMTP (outbound)

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--smtp-host` | `RNS_EMAIL_SMTP_HOST` | *(required)* | SMTP server hostname |
| `--smtp-port` | `RNS_EMAIL_SMTP_PORT` | `587` | SMTP server port |
| `--smtp-username` | `RNS_EMAIL_SMTP_USERNAME` | *(required)* | SMTP login |
| `--smtp-password` | `RNS_EMAIL_SMTP_PASSWORD` | *(required)* | SMTP password (visible in `ps`; prefer file) |
| `--smtp-password-file` | `RNS_EMAIL_SMTP_PASSWORD_FILE` | — | Path to file containing SMTP password (first line) |
| `--smtp-from` | `RNS_EMAIL_SMTP_FROM` | *(required)* | Envelope From address |
| `--smtp-tls` | `RNS_EMAIL_SMTP_TLS` | `starttls` | TLS mode: `tls`, `starttls`, `none` |
| `--smtp-recovery-delay` | `RNS_EMAIL_SMTP_RECOVERY_DELAY` | `300` | Base backoff (seconds) before probing SMTP after send failure |
| `--smtp-max-recovery-delay` | `RNS_EMAIL_SMTP_MAX_RECOVERY_DELAY` | `1800` | Max backoff (seconds) for SMTP recovery probes |

### IMAP (inbound)

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--imap-host` | `RNS_EMAIL_IMAP_HOST` | *(required)* | IMAP server hostname |
| `--imap-port` | `RNS_EMAIL_IMAP_PORT` | `993` | IMAP server port |
| `--imap-username` | `RNS_EMAIL_IMAP_USERNAME` | *(required)* | IMAP login |
| `--imap-password` | `RNS_EMAIL_IMAP_PASSWORD` | *(required)* | IMAP password (visible in `ps`; prefer file) |
| `--imap-password-file` | `RNS_EMAIL_IMAP_PASSWORD_FILE` | — | Path to file containing IMAP password (first line) |
| `--imap-folder` | `RNS_EMAIL_IMAP_FOLDER` | `INBOX` | Mailbox folder to watch |
| `--imap-tls` | `RNS_EMAIL_IMAP_TLS` | `tls` | TLS mode: `tls`, `starttls`, `none` |
| `--imap-poll-interval` | `RNS_EMAIL_IMAP_POLL_INTERVAL` | `60s` | Poll interval when IDLE is unavailable (Go duration, e.g. `30s`) |
| `--imap-reconnect-delay` | `RNS_EMAIL_IMAP_RECONNECT_DELAY` | `5` | Base reconnect backoff (seconds) after session failure |
| `--imap-max-reconnect-delay` | `RNS_EMAIL_IMAP_MAX_RECONNECT_DELAY` | `300` | Max reconnect backoff (seconds); grows exponentially on dial errors |
| `--imap-cleanup-mode` | `RNS_EMAIL_IMAP_CLEANUP_MODE` | `none` | Post-process cleanup: `none`, `delete`, `move`. `delete` requires UIDPLUS (RFC 4315) or IMAP4rev2; without either, delete-cleanup is skipped with a warning log. `move` requires MOVE (RFC 6851) or UIDPLUS (RFC 4315); without either, move-cleanup is skipped with a warning log to prevent unsafe plain EXPUNGE. |
| `--imap-cleanup-target-folder` | `RNS_EMAIL_IMAP_CLEANUP_TARGET_FOLDER` | — | Destination folder for `move` cleanup mode |

### Peer

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--peer-email` | `RNS_EMAIL_PEER_EMAIL` | *(required)* | Email address of the remote RNS peer |

### Checkpoint

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--checkpoint-path` | `RNS_EMAIL_CHECKPOINT_PATH` | `./checkpoint-{name}-{hash}.json` | Path to IMAP UID watermark file (auto-derived from pipe name) |

### Logging

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--log-level` | `RNS_EMAIL_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `--log-format` | `RNS_EMAIL_LOG_FORMAT` | `text` | Log format: `text`, `json` |

### Password security

CLI `--smtp-password` and `--imap-password` flags are visible in `ps aux` and `/proc/PID/cmdline`. For production use, prefer:

- `--smtp-password-file` / `--imap-password-file` — reads the first line of a file
- Environment variables `RNS_EMAIL_SMTP_PASSWORD` / `RNS_EMAIL_IMAP_PASSWORD` — not visible in process listings

---

## Build & Test

```sh
go build ./cmd/rns-over-email
go test ./...
```

---

## Dependencies

| Module | Version | Author | License | Purpose |
|--------|---------|--------|---------|---------|
| `github.com/emersion/go-imap/v2` | `v2.0.0-beta.8` | Simon Ser (emersion) | MIT | IMAP client (IDLE, UID fetch, move/delete) |
| `github.com/emersion/go-sasl` | `v0.0.0-20241020182733` | Simon Ser (emersion) | MIT | SASL authentication (PLAIN, LOGIN) for SMTP/IMAP |
| `github.com/emersion/go-smtp` | `v0.24.0` | Simon Ser (emersion) et al. | MIT | SMTP client with STARTTLS/TLS support |
| `github.com/google/uuid` | `v1.6.0` | Google Inc. | BSD-3-Clause | Message-ID generation for outbound emails |
| `github.com/x3ps/go-rns-pipe` | `v0.1.1` | x3ps | MIT | HDLC-framed stdin/stdout pipe protocol for rnsd |
| `github.com/emersion/go-message` | `v0.18.2` | Simon Ser (emersion) | MIT | MIME message encoding/decoding *(indirect)* |

> **Note on `go-imap/v2` beta**: no stable v2 release exists upstream; `v2.0.0-beta.x` is the de facto current API and is used intentionally.

---

## License

MIT — Copyright (c) 2026 x3ps. See [LICENSE](LICENSE).
