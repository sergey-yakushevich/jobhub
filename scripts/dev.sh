#!/bin/sh
# Local-only preview. Never sources the existing provider credential .env.
set -eu
cd "$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
umask 077
mkdir -p .local
if [ ! -f .env.local ]; then
  python3 - <<'PY'
import os, secrets
from pathlib import Path
p = Path('.env.local')
fd = os.open(p, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, 'w') as f:
    f.write('JOBHUB_API_KEY=jh_' + secrets.token_urlsafe(32) + '\n')
    f.write('HOST=127.0.0.1\nPORT=8096\nBASE_URL=http://127.0.0.1:8096\nDB_PATH=.local/preview.db\nJOBHUB_DEMO=1\n')
print('Created private .env.local (key not printed).')
PY
fi
chmod 600 .env.local
set -a
. ./.env.local
set +a
go build -o .local/jobhub ./cmd/jobhub
exec .local/jobhub
