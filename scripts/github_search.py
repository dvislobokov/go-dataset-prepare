#!/usr/bin/env python3
"""Collect Go candidate repositories from the GitHub Search API (metadata only).

Output: data/go-search.jsonl with the same record fields as the C# pipeline's data/csharp-search.jsonl:
  full_name, stars, pushed_at, size, license, topics, description, owner, owner_type, default_branch

Query: language:Go fork:false archived:false stars:A..B pushed:>=CUTOFF
The search API returns at most 1000 results per query, so the star range is split adaptively (binary split on
stars; a single star value that still exceeds 1000 is split by pushed-date windows). Rate limit: the search API
allows 30 requests/minute for an authenticated token; we pace at >= 2.1 s/request and honour X-RateLimit-Reset /
Retry-After on 403/429.

The token is read from a file (default ~/GITHUB_TOKEN), sent only in the Authorization header, never printed or
written anywhere. Descriptions/topics are untrusted data and are stored verbatim, never interpreted.

Usage: python3 -I scripts/github_search.py [--min-stars 100] [--pushed-since 2024-10-09] [--out data/go-search.jsonl]
Resumable: queries already completed are recorded in <out>.queries.log and skipped on restart.
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

API = "https://api.github.com/search/repositories"
MIN_INTERVAL = 2.1  # seconds between search requests (30/min limit)
_last = [0.0]


def log(msg: str) -> None:
    print(msg, file=sys.stderr, flush=True)


def request(token: str, q: str, page: int, per_page: int = 100) -> dict:
    params = urllib.parse.urlencode({"q": q, "sort": "stars", "order": "desc", "per_page": per_page, "page": page})
    url = f"{API}?{params}"
    for attempt in range(8):
        wait = MIN_INTERVAL - (time.monotonic() - _last[0])
        if wait > 0:
            time.sleep(wait)
        _last[0] = time.monotonic()
        req = urllib.request.Request(url, headers={
            "Authorization": f"Bearer {token}",
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28",
            "User-Agent": "go-flc-dataset-selection",
        })
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                remaining = resp.headers.get("X-RateLimit-Remaining")
                reset = resp.headers.get("X-RateLimit-Reset")
                body = json.loads(resp.read().decode("utf-8"))
                if remaining is not None and int(remaining) <= 1 and reset:
                    sleep = max(0, int(reset) - int(time.time())) + 2
                    log(f"  rate limit nearly exhausted; sleeping {sleep}s")
                    time.sleep(sleep)
                return body
        except urllib.error.HTTPError as e:
            if e.code in (403, 429):
                ra = e.headers.get("Retry-After")
                reset = e.headers.get("X-RateLimit-Reset")
                if ra:
                    sleep = int(ra) + 1
                elif reset:
                    sleep = max(1, int(reset) - int(time.time())) + 2
                else:
                    sleep = 30 * (attempt + 1)
                log(f"  HTTP {e.code}; sleeping {sleep}s (attempt {attempt + 1})")
                time.sleep(sleep)
                continue
            if e.code == 422:  # e.g. page beyond 1000 results
                return {"total_count": 0, "items": [], "incomplete_results": False}
            log(f"  HTTP {e.code}; retry in {5 * (attempt + 1)}s")
            time.sleep(5 * (attempt + 1))
        except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
            log(f"  network error {type(e).__name__}; retry in {5 * (attempt + 1)}s")
            time.sleep(5 * (attempt + 1))
    raise RuntimeError("search request failed after retries")


def record(it: dict) -> dict:
    lic = it.get("license") or {}
    return {
        "full_name": it["full_name"],
        "stars": it["stargazers_count"],
        "pushed_at": it["pushed_at"],
        "size": it["size"],
        "license": lic.get("spdx_id"),
        "topics": it.get("topics") or [],
        "description": it.get("description"),
        "owner": it["owner"]["login"],
        "owner_type": it["owner"]["type"],
        "default_branch": it.get("default_branch"),
    }


def query(lo: int, hi: int | None, since: str, until: str | None) -> str:
    stars = f"stars:{lo}..{hi}" if hi is not None else f"stars:>={lo}"
    pushed = f"pushed:{since}..{until}" if until else f"pushed:>={since}"
    return f"language:Go fork:false archived:false {stars} {pushed}"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--token-file", default=os.path.expanduser("~/GITHUB_TOKEN"))
    ap.add_argument("--min-stars", type=int, default=100)
    ap.add_argument("--max-stars", type=int, default=200000)
    ap.add_argument("--pushed-since", default="2024-10-09")
    ap.add_argument("--out", default="data/go-search.jsonl")
    args = ap.parse_args()

    with open(args.token_file, encoding="utf-8") as f:
        token = f.read().strip()
    if not token:
        log("empty token file")
        return 2

    done_log = args.out + ".queries.log"
    done = set()
    if os.path.exists(done_log):
        with open(done_log, encoding="utf-8") as f:
            done = {json.loads(line)["q"] for line in f if line.strip()}
    out = open(args.out, "a", encoding="utf-8")
    dlog = open(done_log, "a", encoding="utf-8")

    today = dt.date.today().isoformat()
    # work stack of (lo, hi, since, until)
    stack: list[tuple[int, int, str, str | None]] = [(args.min_stars, args.max_stars, args.pushed_since, None)]
    nreq = 0
    while stack:
        lo, hi, since, until = stack.pop()
        q = query(lo, hi, since, until)
        if q in done:
            continue
        first = request(token, q, 1)
        nreq += 1
        total = first.get("total_count", 0)
        if total > 1000:
            if hi > lo:
                mid = (lo + hi) // 2
                log(f"split stars {lo}..{hi} (total {total})")
                stack.append((mid + 1, hi, since, until))
                stack.append((lo, mid, since, until))
                continue
            # single star value: split the pushed-date window
            d0 = dt.date.fromisoformat(since)
            d1 = dt.date.fromisoformat(until) if until else dt.date.fromisoformat(today)
            if d1 > d0:
                dm = d0 + (d1 - d0) // 2
                log(f"split pushed {since}..{until or today} at stars={lo} (total {total})")
                stack.append(((lo), hi, (dm + dt.timedelta(days=1)).isoformat(), until))
                stack.append((lo, hi, since, dm.isoformat()))
                continue
            log(f"WARNING: cannot split further: {q} total={total}; collecting first 1000")
        items = list(first.get("items", []))
        incomplete = bool(first.get("incomplete_results"))
        page = 1
        while len(items) < min(total, 1000) and page < 10:
            page += 1
            res = request(token, q, page)
            nreq += 1
            incomplete |= bool(res.get("incomplete_results"))
            got = res.get("items", [])
            if not got:
                break
            items.extend(got)
        for it in items:
            out.write(json.dumps(record(it), ensure_ascii=False) + "\n")
        out.flush()
        dlog.write(json.dumps({"q": q, "total": total, "got": len(items), "incomplete": incomplete}) + "\n")
        dlog.flush()
        log(f"{q} -> total {total}, got {len(items)}{' (incomplete_results)' if incomplete else ''}; requests {nreq}")
    out.close()
    dlog.close()
    log(f"done; requests this session: {nreq}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
