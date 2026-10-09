# Go repository selection report

Metadata-only selection over `data/go-search.jsonl` (collected from the GitHub Search API: `language:Go fork:false archived:false stars:>=100 pushed:>=2024-10-09`, sliced by star ranges to beat the 1000-results-per-query cap). No repository was cloned, fetched, or executed; descriptions were treated as untrusted data and only pattern-matched. Reproduce with `python3 -I data/selection/selection_script.py`.

## Funnel

- Input rows: **12409**
- After exact-duplicate-row dedup: **12409** unique `full_name` (0 removed)
- Selected: **8951** (in 8951 dedup groups)
- Rejected: **3458**
  - license_unknown bucket: **1530**
  - restricted_license bucket: **1443**

## Rejection reasons (a repo may carry several)

| reason | count |
| --- | ---: |
| license_unknown | 1530 |
| restricted_license | 1443 |
| owner_cap | 344 |
| duplicate_of_group | 95 |
| noncode_or_list | 60 |
| offensive_tooling | 24 |
| abuse_strong | 11 |
| manual_exclude | 8 |
| giant_repo | 7 |
| tiny_lowsignal | 7 |

## Thresholds & justification (from observed distributions)

- **License gate**: permissive allowlist (0BSD, Apache-2.0, BSD-2-Clause, BSD-3-Clause, ISC, MIT, MS-PL, Unlicense, Zlib). Copyleft/reciprocal (GPL/LGPL/AGPL/MPL/EPL, BSD-4-Clause, ...) -> restricted. NOASSERTION/null and permissive-ish ids not on the allowlist (MIT-0, CC0-1.0, WTFPL, BSL-1.0, UPL-1.0, PostgreSQL, ...) -> license_unknown for manual verification. Observed: MIT and Apache-2.0 dominate, with a large NOASSERTION/null tail, so the unknown bucket is intentionally large.
- **Activity cutoff** = 2024-10-09 (the search window). All rows pass today; recency instead feeds the priority score. Kept as an explicit gate for the future 34k manifest.
- **Size** (KB incl. git history; p5=79, median=4390, p90~80k). Reject `< 20 KB AND no description AND no topics` (trivial) and `> 4000000 KB` (giant_repo: asset/data dumps or generated SDK mono-repos where Go is a minority of bytes). Flag `< 300 KB` as tiny and `> 500000 KB` as large (kept; e.g. kubernetes, teleport).
- **Owner cap** = 15 selected repos/owner (keep highest priority). Prevents hashicorp, google, kubernetes-sigs, etc. from dominating.
- **Abuse/content filter**: strong attack-tooling signals (stealers, keyloggers, ransomware, botnets, game cheats, key generators, miner proxies) -> rejected (abuse_strong); repositories combining two or more generic offensive-security signals -> offensive_tooling. Defensive/detection/analysis/firewall/DI contexts neutralise a hit. A single generic signal only lowers the priority score (offensive_security_flag).
- **Go standard library / toolchain**: `golang/go`, gccgo, and vendor forks of the compiler are manual_excluded (compiler, not application code). `golang.org/x/...` mirrors and other std forks are merged into one dedup group so no near-duplicate standard-library code leaks across splits (std_fork_risk).
- **Non-code**: awesome-lists, roadmaps, books, wordlists and proxy/subscription (V2Ray/VMess) dumps -> noncode_or_list. Educational repos are KEPT but flagged and down-weighted.
- **Dedup/grouping**: union-find over (a) identical normalized description len>=40 and (b) same repo name + description Jaccard>=0.5; canonical = highest priority (org over user on ties); others -> duplicate_of_group with a shared `group` id for split isolation.

## Selection — category distribution

| category | count |
| --- | ---: |
| other | 2293 |
| cloud/k8s | 1690 |
| web/api | 987 |
| library/sdk | 654 |
| cli/tui | 623 |
| data/db | 490 |
| networking | 487 |
| ml/ai | 444 |
| security | 387 |
| devtools | 363 |
| observability | 234 |
| blockchain | 105 |
| testing | 105 |
| desktop/gui | 89 |

## Selection — license distribution

| license | count |
| --- | ---: |
| MIT | 4915 |
| Apache-2.0 | 3458 |
| BSD-3-Clause | 351 |
| BSD-2-Clause | 122 |
| ISC | 52 |
| Unlicense | 39 |
| Zlib | 7 |
| 0BSD | 7 |

## Selection — top owners (after cap)

| owner | count |
| --- | ---: |
| kubernetes | 15 |
| microsoft | 15 |
| prometheus | 15 |
| charmbracelet | 15 |
| aquasecurity | 15 |
| docker | 15 |
| projectdiscovery | 15 |
| nats-io | 15 |
| apache | 15 |
| GoogleCloudPlatform | 15 |
| google | 15 |
| pion | 15 |
| kubernetes-sigs | 15 |
| cloudwego | 15 |
| cloudflare | 15 |
| openshift | 15 |
| DataDog | 15 |
| grafana | 15 |
| prometheus-community | 15 |
| aws | 15 |

## Selection — quality flags

| flag | count |
| --- | ---: |
| tiny | 1536 |
| no_desc_no_topics | 166 |
| educational | 116 |
| large | 73 |
| offensive_security_flag | 57 |

## Known limitations (metadata-only)

- **Language share unverified**: GitHub `size` includes all files/history; Go may not be the majority of bytes. The extractor's discovery stage measures accepted bytes per repo at ingestion.
- **Fork/archived status**: `fork:false archived:false` was applied at query time, but fork lineage across owners is not in the data (`fork_of` is null); detect and regroup at clone time.
- **Real license unverified**: the SPDX id is GitHub's guess; the LICENSE file and per-file headers must be confirmed before any repo enters the training corpus. The extractor re-detects the license from LICENSE text and gates per file.
- **Generated code**: `.pb.go`/`_gen.go`/`zz_generated` and `// Code generated ... DO NOT EDIT.` files cannot be seen from metadata; excluded later by the extraction file filters.
