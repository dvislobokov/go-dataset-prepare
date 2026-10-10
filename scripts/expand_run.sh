#!/usr/bin/env bash
# Process every searched Go star bucket (5..99): samples pass and corpus pass in parallel, tag g2. Rounds repeat until the
# search is done; repositories already in a state database are ignored, eval-fresh repositories are excluded.
set -uo pipefail
S=/srv/flc/search-go; PY=/srv/flc/venv/bin/python; APP=/srv/flc-go/go-dataset
cd $APP
export GOROOT=/opt/go PATH=/opt/go/bin:$PATH
while true; do
  search_done=$(grep -c search_done $S/expand.log 2>/dev/null || true)
  m=$S/manifest.round.jsonl
  $PY -I - $S /srv/flc-go/eval-fresh/manifest.jsonl > $m <<'PY'
import glob, json, os, sys
s, ev = sys.argv[1], sys.argv[2]
excl = {json.loads(l)["repository_id"] for l in open(ev)}
seen = set()
for d in sorted(glob.glob(os.path.join(s, "selection_*"))):
    tag = os.path.basename(d)[len("selection_"):]
    if not os.path.exists(os.path.join(s, f"stars_{tag}.done")): continue
    for l in open(os.path.join(d, "selected.jsonl")):
        r = json.loads(l)
        if r["repository_id"] in excl or r["repository_id"] in seen: continue
        seen.add(r["repository_id"]); print(json.dumps(r, ensure_ascii=False))
PY
  echo "{\"event\": \"round\", \"repos\": $(wc -l < $m), \"search_done\": $search_done}" >> $S/expand.log
  $PY -I scripts/goflc_run.py --manifest $m --jobs 56 --workers 2 --batch-samples 400000 --upload-parallel 3 --run-tag g2 \
      >> /srv/flc-go/logs/stdout.log 2>&1 &
  a=$!
  $PY -I scripts/goflc_run.py --manifest $m --jobs 24 --workers 2 --corpus-only --run-tag g2 --upload-parallel 3 \
      --work /srv/flc-go/corpus/work --out /srv/flc-go/corpus/out --state /srv/flc-go/corpus/state.sqlite --logs /srv/flc-go/corpus/logs \
      >> /srv/flc-go/corpus/stdout.log 2>&1 &
  b=$!
  wait $a $b
  [ "$search_done" -ge 1 ] && { echo '{"event": "all_processed"}' >> $S/expand.log; break; }
  until grep -q search_done $S/expand.log; do sleep 120; done
done
