# Go FLC pilot benchmark

Every number is parsed from a real `run-manifest.json`; warm runs reported. See `benchmark.json`.

## ttlcache
- revision `230d2c7db17f98285e293c7c4c82f4c97c976050`, license MIT, 18 files (5 test), 1676 samples

| experiment | workers | wall ms | samples/s | MiB/s | peak RSS MB | validate | sem records |
| --- | ---: | ---: | ---: | ---: | ---: | :---: | ---: |
| E1_syntax_seq | 1 | 63 | 26791 | 1.638 | 21 | True | 0 |
| E1_syntax_par | 8 | 47 | 35468 | 2.168 | 30 | True | 0 |
| E2_semantic_subset | 8 | 2138 | 784 | 0.048 | 305 | True | 488 |
| E3_semantic_full | 8 | 10918 | 154 | 0.009 | 1418 | True | 3352 |

**E4 leakage/policy:** editor_snapshot resolved 455 / partial 1205; strict_prefix resolved 451 / partial 1209; target-identifier overlap editor=1127 strict=1081; **leakage violations 0** (validate ok=True).

## httpmock
- revision `ae9b91658ce4ddcfc8b5950a87de488384d0cff1`, license MIT, 24 files (13 test), 2151 samples

| experiment | workers | wall ms | samples/s | MiB/s | peak RSS MB | validate | sem records |
| --- | ---: | ---: | ---: | ---: | ---: | :---: | ---: |
| E1_syntax_seq | 1 | 76 | 28470 | 2.479 | 22 | True | 0 |
| E1_syntax_par | 8 | 52 | 41137 | 3.582 | 23 | True | 0 |
| E2_semantic_subset | 8 | 2668 | 806 | 0.070 | 272 | True | 674 |
| E3_semantic_full | 8 | 13060 | 165 | 0.014 | 1078 | True | 4302 |

**E4 leakage/policy:** editor_snapshot resolved 1065 / partial 1076; strict_prefix resolved 1066 / partial 1075; target-identifier overlap editor=1348 strict=1369; **leakage violations 0** (validate ok=True).
