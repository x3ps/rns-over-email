# Docker Compose Demo

Self-contained demo of two RNS nodes communicating over email via
[meshchat](https://github.com/liamcottle/reticulum-meshchat) web UI, using
[GreenMail](https://greenmail-mail-test.github.io/greenmail/) as a local
SMTP/IMAP server.

## Prerequisites

- Docker Engine 20.10+
- Docker Compose v2

## Quick Start

```sh
cd example
docker compose up --build
```

The first build downloads Go, Node.js, and Python base images, compiles
`rns-over-email`, and builds the meshchat frontend — this takes a few
minutes. Subsequent runs use the cache.

Once running, open the web panels:

- **Alice (node-a):** <http://localhost:8001>
- **Bob (node-b):** <http://localhost:8002>

## What To Expect

1. **mail** — GreenMail starts, listening on ports 3025 (SMTP) and 3143 (IMAP).
2. **node-a / node-b** — meshchat starts, initializes Reticulum with the
   email PipeInterface. Look for `pipe status online=true` in the logs.
3. The meshchat web UI shows each node's LXMF address. Copy one address and
   use it in the other node's UI to send a message.

## Sending a Message

1. Open <http://localhost:8001> (Alice).
2. Copy Alice's LXMF address from the UI.
3. Open <http://localhost:8002> (Bob).
4. Start a new conversation with Alice's address and send a message.
5. The message travels: Bob's meshchat → Reticulum PipeInterface →
   rns-over-email → SMTP → GreenMail → IMAP → rns-over-email → Reticulum
   → Alice's meshchat.

## Secrets

Email passwords are injected via Docker Compose secrets using the
`--smtp-password-file` / `--imap-password-file` flags. The secret files
live in `secrets/alice.txt` and `secrets/bob.txt`.

These are **fictitious demo credentials** (`alice@demo.local`,
`bob@demo.local`) committed to the repo — GreenMail auto-creates accounts
on first authentication. In a real deployment you would use actual secret
management and never commit credential files.

## Teardown

```sh
docker compose down
```

## Caveats

- **No TLS.** All SMTP/IMAP traffic is plaintext inside the Docker network.
  This is fine for a local demo but not suitable for production.
- **GreenMail auth is disabled.** The demo mail server runs with
  `-Dgreenmail.auth.disabled` by default and accepts any credentials. The
  `--password-file` flags demonstrate correct application-side practice,
  but the server does not enforce authentication.
- **Upstream Python deps not fully pinned.** meshchat's `requirements.txt`
  uses `>=` constraints, so pip may resolve different versions on rebuild.
- **Base images pinned by digest** as of 2026-03-23. Update the SHA256
  digests in `Dockerfile.node` and `docker-compose.yml` to pull newer
  images.
