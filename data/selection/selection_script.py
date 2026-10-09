#!/usr/bin/env python3
"""Deterministic, metadata-only repository selection for the Go FLC dataset.

Adapted from the C# pipeline's data/selection/selection_script.py: same output files, record fields, priority
formula, owner cap and union-find grouping. Go-specific rules are documented in report.md. NO network access;
descriptions/topics are untrusted data and are only pattern-matched. Pure and deterministic (same input ->
byte-identical outputs). Run with: python3 -I data/selection/selection_script.py
"""
import collections
import datetime
import itertools
import json
import math
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
IN = sys.argv[1] if len(sys.argv) > 1 else os.path.join(HERE, "..", "go-search.jsonl")
OUT = sys.argv[2] if len(sys.argv) > 2 else HERE

PERMISSIVE = {"MIT", "Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "0BSD", "Unlicense", "ISC", "MS-PL", "Zlib"}
RESTRICTED_PREFIX = ("GPL", "LGPL", "AGPL", "MPL", "EPL", "OSL", "EUPL", "MS-RL", "CC-BY-SA", "MulanPSL",
                     "BSD-4-Clause", "Artistic", "ODbL")


def license_bucket(lic):
    if lic in PERMISSIVE:
        return "permissive"
    if lic and lic.startswith(RESTRICTED_PREFIX):
        return "restricted"
    return "unknown"  # NOASSERTION/null/permissive-ish-but-not-allowlisted -> manual verification


# Content-exclusion filter. We keep defensive security, detection, analysis, firewall, forensics and pentest-
# reporting tooling (legitimate and common in Go), and down-rank or drop repositories whose described primary
# purpose is running an attack. STRONG terms (primary purpose is abuse/attack) route to a reject bucket; WEAK terms
# (generic offensive-security vocabulary) only lower the priority score, because they also occur in defensive tools.
ABUSE_STRONG = [r"\baimbot\b", r"\bwallhack\b", r"game-?hacking", r"\bstealer\b", r"\bkeylogger\b",
                r"wallet-?drainer", r"\bransomware\b", r"\bbotnet\b", r"free-?robux", r"hwid-?spoof",
                r"\bkeygen\b", r"\bcracked\b", r"miner-?proxy", r"skin-?changer"]
ABUSE_WEAK = [r"\bc2\b", r"command[- ]and[- ]control", r"cobalt-?strike", r"\bshellcode\b", r"\bpayloads?\b",
              r"\bevasion\b", r"av-?bypass", r"\bamsi-?bypass\b", r"\bedr-?bypass\b", r"\bprivesc\b",
              r"privilege-?escalation", r"lateral-?movement", r"\bexfiltrat", r"\bphishing\b", r"\bbackdoor\b",
              r"\brootkit\b", r"\bred-?team(ing)?\b", r"\bddos\b", r"\bimplant\b"]
# Benign contexts that neutralise a STRONG hit (defensive / analysis / DI).
ABUSE_BENIGN = [r"anti-?cheat", r"cheat-?sheet", r"malware-analysis", r"anti-malware", r"detection",
                r"dependency-inject", r"license management", r"license activation", r"key generator for"]
ABUSE_STRONG_RE = [re.compile(p) for p in ABUSE_STRONG]
ABUSE_WEAK_RE = [re.compile(p) for p in ABUSE_WEAK]
ABUSE_BENIGN_RE = [re.compile(p) for p in ABUSE_BENIGN]


def abuse(hay, topics):
    benign = any(r.search(hay) for r in ABUSE_BENIGN_RE)
    strong = [r.pattern for r in ABUSE_STRONG_RE if r.search(hay)]
    weak = [r.pattern for r in ABUSE_WEAK_RE if r.search(hay)]
    reasons, flags = [], []
    if strong and not benign:
        reasons.append("abuse_strong")
    # two or more distinct generic offensive-security signals -> treat as attack tooling, else only flag
    if len(weak) >= 2 and not benign:
        reasons.append("offensive_tooling")
    elif weak:
        flags.append("offensive_security_flag")
    return reasons, flags


# Non-code / low-signal: curated lists, books, slide decks, config/subscription/wordlist dumps (common in Go search
# noise: V2Ray/proxy subscription repos, algorithm-course repos).
NONCODE_NAME = re.compile(r"(^awesome-|-awesome$|roadmap|interview|cheat-?sheet|wordlist|\bbooks?\b)", re.I)
NONCODE_DESC = re.compile(r"(curated list|awesome list|a collection of (awesome|resources|links)|free.?nodes?|"
                          r"subscription (links?|nodes?)|机场|订阅|presentation(s)? (from|repo)|slides? (deck|from)|"
                          r"vmess|vless.*subscription|free v2ray)", re.I)
EDU = re.compile(r"(tutorial|\bcourse\b|bootcamp|\blearn(ing)?\b|homework|\bexercise(s)?\b|\bkata\b|leetcode|"
                 r"getting-started|for beginners|study-guide|algorithms and data structures)", re.I)
STD_FORK = re.compile(r"(\[mirror\]|standard library|supplementary (go )?(network|image|time|crypto)|"
                      r"go supplementary|fork of go/x/|fork of the go standard)", re.I)

THROWAWAY_OWNER = re.compile(r"^[a-z]{5,}\d{2,}-(blip|bit|hue|boop|dotcom|coder|source|ltd|dev|gen|user|tmp)$", re.I)


def looks_gibberish(owner):
    o = owner.lower()
    if THROWAWAY_OWNER.match(o):
        return True
    stem = re.sub(r"[-_0-9]", "", o)
    if len(stem) >= 10 and re.search(r"\d{3,}", o):
        if sum(c in "aeiou" for c in stem) / len(stem) < 0.22:
            return True
    return False


# Size thresholds. size is repo KB incl. git history. Go distribution: p5=79, median=4390, p90=79602, max=23M KB.
TINY_FLAG_KB = 300
TINY_REJECT_KB = 20          # < 20 KB AND no description AND no topics -> trivial
LARGE_FLAG_KB = 500_000
GIANT_KB = 4_000_000         # > ~4 GB incl. history -> giant_repo (asset/data dump or SDK mono-repo); documented reject

ACTIVITY_CUTOFF = "2024-10-09T00:00:00Z"  # matches the search window; kept as an explicit gate for the 34k manifest
DATE_MIN, DATE_MAX = "2024-10-09", "2026-10-09"

# Manual exclusions: Go toolchain forks (not ordinary project code) and known data dumps. Kept as data for
# reproducibility; reason code manual_exclude. golang/go itself is excluded (compiler, not application code).
MANUAL_EXCLUDE = {
    "golang/go", "cloudflare/go", "microsoft/go", "golang/gofrontend", "thongtech/go-legacy-win7",
    "aws/aws-cdk-go", "Danialsamadi/v2go", "dotabuff/d2vpkr",
}


def to_days(ts):
    return (datetime.date.fromisoformat(ts[:10]) - datetime.date(2024, 1, 1)).days


# Category mapping (first match wins; ordered specific -> generic). Go-flavoured.
CATEGORY_RULES = [
    ("devtools",     r"\blinter\b|\banalyzer(s)?\b|code-?gen|language-server|\blsp\b|\bcompiler\b|\bformatter\b|generator|\bast\b"),
    ("cloud/k8s",    r"kubernetes|\bk8s\b|\bk3s\b|operator|\bhelm\b|\bistio\b|service-?mesh|container|\bdocker\b|\boci\b|cloud-native|serverless|terraform|crossplane"),
    ("web/api",      r"\bhttp\b|\brest\b|web-?framework|\bgin\b|\becho\b|\bfiber\b|\brouter\b|graphql|\bapi\b|middleware|openapi|swagger"),
    ("networking",   r"\btcp\b|\budp\b|websocket|\bgrpc\b|\bproxy\b|\btunnel\b|\bvpn\b|\bp2p\b|\bdns\b|\bmqtt\b|\bquic\b|load-?balanc|networking|\bsocket\b"),
    ("data/db",      r"\bdatabase\b|\bsql\b|postgres|\bmysql\b|sqlite|\bredis\b|mongodb|\borm\b|key-?value|\bkv\b|time-?series|\betcd\b|\bkafka\b|message-?queue|streaming"),
    ("security",    r"\bsecurity\b|cryptograph|\boauth\b|\bjwt\b|authentication|\bidentity\b|\bpentest\b|\binfosec\b|vulnerab|\bscanner\b|\bsecrets?\b|\bwaf\b|forensic|firewall"),
    ("ml/ai",        r"\bai\b|\bllm\b|machine-learning|deep-learning|\bonnx\b|\bopenai\b|\bagent\b|\bmcp\b|model-context-protocol|\brag\b|embedding"),
    ("observability", r"observability|\bmetrics\b|\btracing\b|\blogging\b|prometheus|\bgrafana\b|telemetry|monitoring|\bopentelemetry\b"),
    ("cli/tui",      r"\bcli\b|command-line|\btui\b|\bterminal\b|\bprompt\b|\bshell\b|\brepl\b"),
    ("blockchain",   r"blockchain|\bethereum\b|\bbitcoin\b|\bweb3\b|smart-?contract|\bcosmos\b|\bevm\b|\bcrypto(currency)?\b|wallet"),
    ("desktop/gui",  r"\bgui\b|\bwails\b|\bfyne\b|\bgio\b|desktop-app|\bgame\b|gamedev|\bebiten\b"),
    ("testing",      r"\btest(ing)?\b|\bmock\b|assertion|\bbenchmark\b|fuzz|\bginkgo\b"),
    ("library/sdk",  r"\blibrary\b|\bsdk\b|\bclient\b|\bwrapper\b|\bbindings?\b|\btoolkit\b|\bframework\b|\butilit(y|ies)\b"),
]
CATEGORY_COMPILED = [(n, re.compile(rx, re.I)) for n, rx in CATEGORY_RULES]


def categorize(hay):
    for name, rx in CATEGORY_COMPILED:
        if rx.search(hay):
            return name
    return "other"


def norm_name(full):
    return re.sub(r"[-_.]", "", full.split("/")[1].lower())


def norm_desc(d):
    return re.sub(r"\s+", " ", re.sub(r"[^\w\s]", "", (d or "").lower())).strip()


STOP = {"a", "an", "the", "for", "and", "of", "to", "in", "with", "is", "go", "golang", "lib", "library", "tool"}


def desc_tokens(d):
    return set(re.findall(r"[a-z0-9]+", (d or "").lower())) - STOP


def jaccard(a, b):
    return len(a & b) / len(a | b) if (a or b) else 0.0


class UF:
    def __init__(self, keys):
        self.p = {k: k for k in keys}

    def find(self, x):
        while self.p[x] != x:
            self.p[x] = self.p[self.p[x]]
            x = self.p[x]
        return x

    def union(self, a, b):
        ra, rb = self.find(a), self.find(b)
        if ra != rb:
            self.p[max(ra, rb)] = min(ra, rb)


def main():
    with open(IN, encoding="utf-8") as f:
        raw = [json.loads(line) for line in f if line.strip()]
    seen, repos = set(), []
    for r in raw:
        if r["full_name"] in seen:
            continue
        seen.add(r["full_name"])
        repos.append(r)
    n_total, n_unique = len(raw), len(repos)

    recs = {}
    for r in repos:
        fn = r["full_name"]
        desc = r["description"] or ""
        hay = (fn + " " + desc + " " + " ".join(r["topics"])).lower()
        flags, reasons = [], []

        lb = license_bucket(r["license"])
        if lb == "restricted":
            reasons.append("restricted_license")
        elif lb == "unknown":
            reasons.append("license_unknown")

        ab_r, ab_f = abuse(hay, r["topics"])
        reasons += ab_r
        flags += ab_f

        if looks_gibberish(r["owner"]) and not desc and not r["topics"]:
            reasons.append("gibberish_lowsignal")
        elif looks_gibberish(r["owner"]):
            flags.append("throwaway_owner_suspect")

        name = fn.split("/")[1]
        if NONCODE_NAME.search(name) or NONCODE_DESC.search(desc):
            reasons.append("noncode_or_list")

        if not desc and not r["topics"]:
            flags.append("no_desc_no_topics")

        edu = bool(EDU.search(hay))
        if edu:
            flags.append("educational")

        if STD_FORK.search(desc) or r["owner"] == "golang":
            flags.append("std_fork_risk")

        sz = r["size"]
        if sz < TINY_REJECT_KB and not desc and not r["topics"]:
            reasons.append("tiny_lowsignal")
        if sz < TINY_FLAG_KB:
            flags.append("tiny")
        if sz > LARGE_FLAG_KB:
            flags.append("large")
        if sz > GIANT_KB:
            reasons.append("giant_repo")

        if r["pushed_at"] < ACTIVITY_CUTOFF:
            reasons.append("inactive")
        if fn in MANUAL_EXCLUDE:
            reasons.append("manual_exclude")

        cat = categorize(hay)
        stars_norm = min(1.0, math.log10(r["stars"] + 1) / math.log10(150000 + 1))
        d0, d1 = to_days(DATE_MIN), to_days(DATE_MAX)
        recency = max(0.0, min(1.0, (to_days(r["pushed_at"]) - d0) / (d1 - d0)))
        owner_bonus = 1.0 if r["owner_type"] == "Organization" else 0.0
        quality = (0.5 if desc else 0.0) + min(0.5, 0.1 * len(r["topics"]))
        if edu:
            quality *= 0.6
        if "offensive_security_flag" in flags:
            quality *= 0.7
        score = 0.45 * stars_norm + 0.30 * recency + 0.10 * owner_bonus + 0.15 * quality
        priority = round(1000 * score)

        recs[fn] = dict(r=r, license_bucket=lb, flags=flags, reasons=reasons, category=cat, priority=priority,
                        ndesc=norm_desc(desc), nname=norm_name(fn), dtok=desc_tokens(desc))

    # grouping / dedup
    uf = UF(list(recs.keys()))
    by_desc = collections.defaultdict(list)
    for fn, rec in recs.items():
        if len(rec["ndesc"]) >= 40:
            by_desc[rec["ndesc"]].append(fn)
    for group in by_desc.values():
        if len(group) > 1:
            g = sorted(group)
            for other in g[1:]:
                uf.union(g[0], other)
    by_name = collections.defaultdict(list)
    for fn, rec in recs.items():
        by_name[rec["nname"]].append(fn)
    for group in by_name.values():
        if len(group) < 2:
            continue
        for a, b in itertools.combinations(sorted(group), 2):
            if jaccard(recs[a]["dtok"], recs[b]["dtok"]) >= 0.5:
                uf.union(a, b)
    # Go standard-library / toolchain mirrors form one group (canonical golang/go, which is itself manual_excluded;
    # the members keep the shared group id so no std fork leaks into a different split than the others).
    std_members = [fn for fn, rec in recs.items() if "std_fork_risk" in rec["flags"]]
    for fn in std_members:
        uf.union("golang/go" if "golang/go" in recs else min(std_members), fn)

    members = collections.defaultdict(list)
    for fn in recs:
        members[uf.find(fn)].append(fn)
    group_id, canonical = {}, {}
    for root in sorted(members):
        g = members[root]
        gid = "grp_" + re.sub(r"[^a-z0-9]", "-", root.lower())
        best = max(g, key=lambda fn: (recs[fn]["priority"], recs[fn]["r"]["stars"],
                                      recs[fn]["r"]["owner_type"] == "Organization", fn))
        for fn in g:
            group_id[fn] = gid
        canonical[root] = best
        for fn in g:
            if fn != best:
                recs[fn]["reasons"].append("duplicate_of_group")

    OWNER_CAP = 15

    def is_selectable(fn):
        return not recs[fn]["reasons"]

    by_owner = collections.defaultdict(list)
    for fn in recs:
        if is_selectable(fn):
            by_owner[recs[fn]["r"]["owner"]].append(fn)
    for owner, fns in by_owner.items():
        if len(fns) <= OWNER_CAP:
            continue
        ranked = sorted(fns, key=lambda fn: (-recs[fn]["priority"], -recs[fn]["r"]["stars"], fn))
        for fn in ranked[OWNER_CAP:]:
            recs[fn]["reasons"].append("owner_cap")

    selected, rejected, unknown_bucket, restricted_bucket = [], [], [], []
    for fn, rec in recs.items():
        r = rec["r"]
        if rec["reasons"]:
            row = {"full_name": fn, "reasons": sorted(set(rec["reasons"]))}
            rejected.append(row)
            if "license_unknown" in row["reasons"]:
                unknown_bucket.append({"full_name": fn, "license": r["license"], "stars": r["stars"], "reasons": row["reasons"]})
            if "restricted_license" in row["reasons"]:
                restricted_bucket.append({"full_name": fn, "license": r["license"], "stars": r["stars"], "reasons": row["reasons"]})
        else:
            selected.append({
                "url": f"https://github.com/{fn}", "repository_id": f"github.com/{fn}", "revision": None,
                "default_branch": r["default_branch"], "license": r["license"], "provenance": "go-search.jsonl",
                "priority": rec["priority"], "fork_of": None, "category": rec["category"], "group": group_id[fn],
                "stars": r["stars"], "pushed_at": r["pushed_at"], "size_kb": r["size"], "flags": rec["flags"],
            })

    selected.sort(key=lambda x: (-x["priority"], x["repository_id"]))
    rejected.sort(key=lambda x: x["full_name"])
    unknown_bucket.sort(key=lambda x: (-x["stars"], x["full_name"]))
    restricted_bucket.sort(key=lambda x: (-x["stars"], x["full_name"]))

    def write_jsonl(name, rows):
        with open(os.path.join(OUT, name), "w", encoding="utf-8") as f:
            for row in rows:
                f.write(json.dumps(row, ensure_ascii=False, sort_keys=True) + "\n")

    write_jsonl("selected.jsonl", selected)
    write_jsonl("rejected.jsonl", rejected)
    write_jsonl("license_unknown.jsonl", unknown_bucket)
    write_jsonl("restricted_license.jsonl", restricted_bucket)

    reason_counts = collections.Counter(rr for row in rejected for rr in row["reasons"])
    cat_counts = collections.Counter(s["category"] for s in selected)
    lic_counts = collections.Counter(s["license"] for s in selected)
    owner_counts = collections.Counter(s["url"].split("/")[3] for s in selected)
    flag_counts = collections.Counter(fl for s in selected for fl in s["flags"])
    n_groups_sel = len({s["group"] for s in selected})

    def md_table(counter, cols=("key", "count"), n=None):
        out = [f"| {cols[0]} | {cols[1]} |", "| --- | ---: |"]
        out += [f"| {k} | {v} |" for k, v in counter.most_common(n)]
        return "\n".join(out)

    rep = []
    rep.append("# Go repository selection report\n")
    rep.append("Metadata-only selection over `data/go-search.jsonl` (collected from the GitHub Search API: "
               "`language:Go fork:false archived:false stars:>=100 pushed:>=2024-10-09`, sliced by star ranges to "
               "beat the 1000-results-per-query cap). No repository was cloned, fetched, or executed; descriptions "
               "were treated as untrusted data and only pattern-matched. Reproduce with "
               "`python3 -I data/selection/selection_script.py`.\n")
    rep.append("## Funnel\n")
    rep.append(f"- Input rows: **{n_total}**\n"
               f"- After exact-duplicate-row dedup: **{n_unique}** unique `full_name` ({n_total - n_unique} removed)\n"
               f"- Selected: **{len(selected)}** (in {n_groups_sel} dedup groups)\n"
               f"- Rejected: **{len(rejected)}**\n"
               f"  - license_unknown bucket: **{len(unknown_bucket)}**\n"
               f"  - restricted_license bucket: **{len(restricted_bucket)}**\n")
    rep.append("## Rejection reasons (a repo may carry several)\n")
    rep.append(md_table(reason_counts, ("reason", "count")) + "\n")
    rep.append("## Thresholds & justification (from observed distributions)\n")
    rep.append(
        f"- **License gate**: permissive allowlist ({', '.join(sorted(PERMISSIVE))}). Copyleft/reciprocal "
        "(GPL/LGPL/AGPL/MPL/EPL, BSD-4-Clause, ...) -> restricted. NOASSERTION/null and permissive-ish ids not on "
        "the allowlist (MIT-0, CC0-1.0, WTFPL, BSL-1.0, UPL-1.0, PostgreSQL, ...) -> license_unknown for manual "
        "verification. Observed: MIT and Apache-2.0 dominate, with a large NOASSERTION/null tail, so the unknown "
        "bucket is intentionally large.\n"
        "- **Activity cutoff** = 2024-10-09 (the search window). All rows pass today; recency instead feeds the "
        "priority score. Kept as an explicit gate for the future 34k manifest.\n"
        f"- **Size** (KB incl. git history; p5=79, median=4390, p90~80k). Reject `< {TINY_REJECT_KB} KB AND no "
        f"description AND no topics` (trivial) and `> {GIANT_KB} KB` (giant_repo: asset/data dumps or generated "
        f"SDK mono-repos where Go is a minority of bytes). Flag `< {TINY_FLAG_KB} KB` as tiny and "
        f"`> {LARGE_FLAG_KB} KB` as large (kept; e.g. kubernetes, teleport).\n"
        f"- **Owner cap** = {OWNER_CAP} selected repos/owner (keep highest priority). Prevents hashicorp, google, "
        "kubernetes-sigs, etc. from dominating.\n"
        "- **Abuse/content filter**: strong attack-tooling signals (stealers, keyloggers, ransomware, botnets, "
        "game cheats, key generators, miner proxies) -> rejected (abuse_strong); repositories combining two or more "
        "generic offensive-security signals -> offensive_tooling. Defensive/detection/analysis/firewall/DI contexts "
        "neutralise a hit. A single generic signal only lowers the priority score (offensive_security_flag).\n"
        "- **Go standard library / toolchain**: `golang/go`, gccgo, and vendor forks of the compiler are "
        "manual_excluded (compiler, not application code). `golang.org/x/...` mirrors and other std forks are merged "
        "into one dedup group so no near-duplicate standard-library code leaks across splits (std_fork_risk).\n"
        "- **Non-code**: awesome-lists, roadmaps, books, wordlists and proxy/subscription (V2Ray/VMess) dumps -> "
        "noncode_or_list. Educational repos are KEPT but flagged and down-weighted.\n"
        "- **Dedup/grouping**: union-find over (a) identical normalized description len>=40 and (b) same repo name + "
        "description Jaccard>=0.5; canonical = highest priority (org over user on ties); others -> "
        "duplicate_of_group with a shared `group` id for split isolation.\n")
    rep.append("## Selection — category distribution\n")
    rep.append(md_table(cat_counts, ("category", "count")) + "\n")
    rep.append("## Selection — license distribution\n")
    rep.append(md_table(lic_counts, ("license", "count")) + "\n")
    rep.append("## Selection — top owners (after cap)\n")
    rep.append(md_table(owner_counts, ("owner", "count"), 20) + "\n")
    rep.append("## Selection — quality flags\n")
    rep.append(md_table(flag_counts, ("flag", "count")) + "\n")
    rep.append("## Known limitations (metadata-only)\n")
    rep.append(
        "- **Language share unverified**: GitHub `size` includes all files/history; Go may not be the majority of "
        "bytes. The extractor's discovery stage measures accepted bytes per repo at ingestion.\n"
        "- **Fork/archived status**: `fork:false archived:false` was applied at query time, but fork lineage across "
        "owners is not in the data (`fork_of` is null); detect and regroup at clone time.\n"
        "- **Real license unverified**: the SPDX id is GitHub's guess; the LICENSE file and per-file headers must be "
        "confirmed before any repo enters the training corpus. The extractor re-detects the license from LICENSE "
        "text and gates per file.\n"
        "- **Generated code**: `.pb.go`/`_gen.go`/`zz_generated` and `// Code generated ... DO NOT EDIT.` files "
        "cannot be seen from metadata; excluded later by the extraction file filters.\n")
    with open(os.path.join(OUT, "report.md"), "w", encoding="utf-8") as f:
        f.write("\n".join(rep))

    # pilot_candidates.md: small, permissive, recently active, moderately sized, one per category.
    def pick(pred, used):
        for s in selected:
            if pred(s) and 200 <= s["size_kb"] <= 60000 and s["pushed_at"] >= "2025-06-01" \
                    and not (set(s["flags"]) & {"large", "offensive_security_flag", "std_fork_risk"}) \
                    and s["url"] not in used:
                return s
        return None

    wanted = [("library/sdk", "library/sdk"), ("cli/tui", "cli/tui"), ("web/api", "web/api"),
              ("devtools", "devtools"), ("data/db", "data/db"), ("networking", "networking"), ("testing", "testing")]
    pm = ["# Pilot shortlist (small, diverse, permissive repos for the next multi-repo pilot)\n",
          "All permissive, recently active, moderately sized. Language share, fork/archived status and the real "
          "LICENSE file must still be verified at checkout.\n"]
    used = set()
    for label, cat in wanted:
        s = pick(lambda x: x["category"] == cat, used)
        if s:
            used.add(s["url"])
            fn = s["url"].split("github.com/")[1]
            pm.append(f"- **{fn}** — {label}; {s['stars']} stars, {s['size_kb']} KB, license {s['license']}, "
                      f"pushed {s['pushed_at'][:10]}, priority {s['priority']}.")
    with open(os.path.join(OUT, "pilot_candidates.md"), "w", encoding="utf-8") as f:
        f.write("\n".join(pm) + "\n")

    return dict(n_total=n_total, n_unique=n_unique, selected=len(selected), rejected=len(rejected),
                unknown=len(unknown_bucket), restricted=len(restricted_bucket), reason_counts=dict(reason_counts),
                cat_counts=dict(cat_counts))


if __name__ == "__main__":
    print(json.dumps(main(), indent=2))
