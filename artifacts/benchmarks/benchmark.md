# Go FLC pilot benchmark

Every number is parsed from a real `run-manifest.json`; warm runs reported. See `benchmark.json`.

## ttlcache
- revision `230d2c7db17f98285e293c7c4c82f4c97c976050`, license MIT, 18 files (5 test), 1676 samples

| experiment | workers | wall ms | samples/s | MiB/s | peak RSS MB | validate | sem records |
| --- | ---: | ---: | ---: | ---: | ---: | :---: | ---: |
| E1_syntax_seq | 1 | 79 | 21221 | 1.297 | 20 | True | 0 |
| E1_syntax_par | 8 | 66 | 25291 | 1.546 | 29 | True | 0 |
| E2_semantic_subset | 8 | 547 | 3064 | 0.187 | 368 | True | 488 |
| E3_semantic_full | 8 | 1001 | 1674 | 0.102 | 957 | True | 3352 |
| E3e_semantic_editor_seq | 1 | 1173 | 1429 | 0.087 | 428 | True | 1676 |
| E3e_semantic_editor_par | 8 | 628 | 2668 | 0.163 | 530 | True | 1676 |

**E4 leakage/policy:** editor_snapshot resolved 455 / partial 1205; strict_prefix resolved 451 / partial 1209; target-identifier overlap editor=1136 strict=1079; **leakage violations 0** (validate ok=True).

## httpmock
- revision `ae9b91658ce4ddcfc8b5950a87de488384d0cff1`, license MIT, 24 files (13 test), 2151 samples

| experiment | workers | wall ms | samples/s | MiB/s | peak RSS MB | validate | sem records |
| --- | ---: | ---: | ---: | ---: | ---: | :---: | ---: |
| E1_syntax_seq | 1 | 103 | 20938 | 1.823 | 22 | True | 0 |
| E1_syntax_par | 8 | 80 | 27026 | 2.354 | 24 | True | 0 |
| E2_semantic_subset | 8 | 562 | 3828 | 0.333 | 239 | True | 674 |
| E3_semantic_full | 8 | 903 | 2383 | 0.208 | 575 | True | 4302 |
| E3e_semantic_editor_seq | 1 | 1080 | 1992 | 0.174 | 333 | True | 2151 |
| E3e_semantic_editor_par | 8 | 604 | 3560 | 0.310 | 333 | True | 2151 |

**E4 leakage/policy:** editor_snapshot resolved 1065 / partial 1076; strict_prefix resolved 1066 / partial 1075; target-identifier overlap editor=1361 strict=1368; **leakage violations 0** (validate ok=True).
