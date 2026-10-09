# Deploying the Go bulk run (Ubuntu server)

Target: Ubuntu 26.04, 128 vCPU, 251 GB RAM, 1 TB disk. Output: Hugging Face dataset `dvislobokov/go-ml-complation`
(public). Paths below are the orchestrator defaults; nothing here touches the C# run under `/srv/flc` except reading
the shared secrets directory and reusing the Python venv.

## 1. Go toolchain into /opt/go (official tarball, checksum verified)

```bash
set -euo pipefail
cd /tmp
VER=$(curl -fsSL 'https://go.dev/dl/?mode=json' | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["version"])')
FILE=$VER.linux-amd64.tar.gz
SHA=$(curl -fsSL 'https://go.dev/dl/?mode=json' | python3 -c "import json,sys; r=json.load(sys.stdin)[0]; print([f['sha256'] for f in r['files'] if f['filename']=='$FILE'][0])")
curl -fsSLO "https://go.dev/dl/$FILE"
echo "$SHA  $FILE" | sha256sum -c -
sudo rm -rf /opt/go && sudo tar -C /opt -xzf "$FILE"      # creates /opt/go
/opt/go/bin/go version
```

The pilot was built and measured with go1.27.2 (stdlib sources in `/opt/go/src` are what the offline semantic tier
type-checks against; keep the toolchain fixed for one run so facts are consistent).

## 2. Build goflc (no network needed: the module has no dependencies)

```bash
sudo mkdir -p /srv/flc-go && sudo chown "$USER" /srv/flc-go
git clone <this repository> /srv/flc-go/go-dataset        # or rsync the directory
cd /srv/flc-go/go-dataset
export PATH=/opt/go/bin:$PATH GOROOT=/opt/go GOTOOLCHAIN=local GOPROXY=off GOFLAGS=-mod=mod
go build -trimpath -o bin/goflc ./cmd/goflc
go test ./...                                             # optional, ~5 s
```

## 3. Python environment and secrets

The orchestrator needs `pyarrow` and `huggingface_hub` (the C# run's venv `/srv/flc/venv` already has both):

```bash
/srv/flc/venv/bin/python -c 'import pyarrow, huggingface_hub; print("ok")'
ls -l /srv/flc/secrets/HF_TOKEN /srv/flc/secrets/GITHUB_TOKEN   # read by the orchestrator, never printed
```

Create the dataset repository once (public) or let the first upload fail fast if it does not exist:

```bash
/srv/flc/venv/bin/python -c 'from huggingface_hub import HfApi; HfApi(token=open("/srv/flc/secrets/HF_TOKEN").read().strip()).create_repo("dvislobokov/go-ml-complation", repo_type="dataset", private=False, exist_ok=True)'
```

## 4. Run

```bash
cd /srv/flc-go/go-dataset
mkdir -p /srv/flc-go/{work,out,state,logs}
# smoke: 20 smallest repositories into a trial folder of the dataset repo
/srv/flc/venv/bin/python -I scripts/goflc_run.py --manifest data/selection/selected.jsonl \
  --limit 20 --path-prefix trial --run-tag trial --jobs 16
# full run (resumable; re-running the same command continues from SQLite state)
nohup /srv/flc/venv/bin/python -I scripts/goflc_run.py --manifest data/selection/selected.jsonl \
  --jobs 60 --workers 2 --rss-limit-gb 16 > /srv/flc-go/logs/nohup.out 2>&1 &
# corpus pass (separate state file so it does not interfere)
/srv/flc/venv/bin/python -I scripts/goflc_run.py --manifest data/selection/selected.jsonl \
  --corpus-only --state /srv/flc-go/state/corpus.sqlite --jobs 60
```

Defaults: `--work /srv/flc-go/work --out /srv/flc-go/out --state /srv/flc-go/state/jobs.sqlite --logs /srv/flc-go/logs
--go-root /opt/go --cli bin/goflc --base-config configs/go.bulk.json --hf-repo dvislobokov/go-ml-complation
--hf-token-file /srv/flc/secrets/HF_TOKEN --github-token-file /srv/flc/secrets/GITHUB_TOKEN`.

Sizing: Go repositories are processed smallest-first with a 10% big lane; each job runs `goflc` with `--workers 2`
under an RSS watchdog (`--rss-limit-gb`, `GOMEMLIMIT` = 3/4 of it). The pilot semantic peak was 0.3–1 GB per small
repository; 60 jobs × 16 GB limit stays inside 251 GB only if most jobs are small — keep `--jobs` × typical RSS well
below RAM and watch `progress` lines (`disk_free_gb`). Checkouts are deleted after each job; local Parquet after each
upload batch (unless `--keep-local`).

Monitoring: `tail -f /srv/flc-go/logs/run.jsonl | grep -E 'progress|batch_uploaded|error'`;
`sqlite3 /srv/flc-go/state/jobs.sqlite 'select status, count(*), sum(samples) from jobs group by status'`.
Stop with SIGTERM/SIGINT (graceful: running jobs return to pending).
