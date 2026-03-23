#!/usr/bin/env bash
set -euo pipefail

host=mail
smtp_port=3025
imap_port=3143

echo "Waiting for mail server at ${host}..."

for port in "$smtp_port" "$imap_port"; do
  until bash -c "echo > /dev/tcp/${host}/${port}" 2>/dev/null; do
    echo "  port ${port} not ready, retrying..."
    sleep 1
  done
  echo "  port ${port} ready"
done

echo "Mail server is up, starting meshchat"
exec python /opt/meshchat/meshchat.py \
  --headless \
  --port 8000 \
  --host 0.0.0.0 \
  --reticulum-config-dir /etc/reticulum \
  --storage-dir /data
