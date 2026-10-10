#!/usr/bin/env bash
# Search GitHub for Go repositories in 5-star buckets 20..99 (highest first) and run the Go selection on each.
set -uo pipefail
S=/srv/flc/search-go; PY=/srv/flc/venv/bin/python
for lo in 95 90 85 80 75 70 65 60 55 50 45 40 35 30 25 20; do
  hi=$((lo + 4)); tag=$(printf "%02d_%02d" $lo $hi)
  [ -f $S/stars_$tag.done ] && continue
  $PY -I /srv/flc/app/scripts/gh_search.py --language Go --stars $lo..$hi --out $S/stars_$tag.jsonl >> $S/search_$tag.log 2>&1
  mkdir -p $S/selection_$tag
  $PY -I /srv/flc-go/go-dataset/data/selection/selection_script.py $S/stars_$tag.jsonl $S/selection_$tag > $S/select_$tag.log 2>&1
  touch $S/stars_$tag.done
  echo "{\"event\": \"bucket_done\", \"stars\": \"$tag\", \"selected\": $(wc -l < $S/selection_$tag/selected.jsonl)}" >> $S/expand.log
done
echo '{"event": "search_done"}' >> $S/expand.log
