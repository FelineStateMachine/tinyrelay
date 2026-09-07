# Git performance

Phase wall times come from the recorded harness. Missing or failed phases remain visible. This report does not compare against the bindws Worker runtime.

Method: Linux Slate, 16 CPUs, 10 GiB test-container ceiling, no CPU cap, no swap; client/server loopback; Git 2.50.1 client and 2.39.5 server; fixed local heads and tags with no cold page-cache reset. Phase CPU deltas are total container CPU inclusive of Git children and background probes, and values below sampler resolution are shown as missing.

Methods: native Git HTTP baseline, buffered Tiny, and streaming Tiny where result artifacts exist.

## Findings

| finding | before | after |
| --- | --- | --- |
| Peak Go memory (MiB) | 6,094 | 40.6 |
| Tag-heavy strudel incremental push (ms) | 1,510 | 94.1 |

| repo | before clone | before concurrent4 | before state ACK | before push | after clone | after concurrent4 | after state ACK | after push |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| atlas.git | 12,971 | — | FAILED: Nostr request timed out after 30 seconds | — | 9,148 | 14,383 | 26.8 | 67 |
| bindws.git | — | — | — | — | 178.0 | 267.5 | 21.3 | 59.4 |
| diagramzip.git | 1,844 | 2,391 | 27.7 | 58.8 | 1,559 | 2,118 | 21.9 | 58.3 |
| doorbearer.git | 5,669 | 7,782 | 18.6 | 60.4 | 4,444 | 6,795 | 24.5 | 59.6 |
| nzip.git | 100.1 | 114.8 | 33.4 | 99.4 | 73.4 | 79.3 | 19.4 | 59.2 |
| strudel.git | 3,052 | 4,933 | 615.6 | 1,510 | 2,035 | 3,747 | 24.3 | 94.1 |

## Remaining relay latency

Every probe succeeded, but durable event acknowledgements still stalled occasionally during large Git transfers, including native Git baseline phases on the same host. These results do not identify the cause. Capture disk latency and scheduling before selecting another optimization.

| workload | ACK P95 ms | ACK max ms | query P95 ms | query max ms |
| --- | --- | --- | --- | --- |
| Public Atlas | 68.3 | 719.3 | 1.71 | 331.1 |
| Native Git Atlas phases | 53.7 | 656.4 | 1.69 | 4.51 |
| Private atlas.git | 86.7 | 1,384 | 2.98 | 33.5 |

## Phase results

| repo | method | clone n | initial push ms | clone median ms | fetch ms | incremental state ACK ms | incremental push ms | concurrent ×2 wall ms | concurrent ×4 wall ms |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| nzip.git | streaming Tiny | 3 | 111.0 | 73.4 | 26.9 | 19.4 | 59.2 | 75.3 | 79.3 |
| nzip.git | native Git HTTP (after) | 3 | 84 | 67.6 | 23.9 | — | 33.6 | 73.1 | 77.6 |
| bindws.git | streaming Tiny | 3 | 249.2 | 178.0 | 27.9 | 21.3 | 59.4 | 203.8 | 267.5 |
| bindws.git | native Git HTTP (after) | 3 | 171.7 | 172.1 | 24.6 | — | 33.4 | 205.5 | 256.1 |
| diagramzip.git | streaming Tiny | 3 | 1,557 | 1,559 | 26.3 | 21.9 | 58.3 | 1,698 | 2,118 |
| diagramzip.git | native Git HTTP (after) | 3 | 1,470 | 1,538 | 24.5 | — | 35.8 | 1,720 | 2,112 |
| strudel.git | streaming Tiny | 3 | 2,595 | 2,035 | 36.1 | 24.3 | 94.1 | 2,625 | 3,747 |
| strudel.git | native Git HTTP (after) | 3 | 2,499 | 2,054 | 31.4 | — | 55.6 | 2,458 | 3,645 |
| doorbearer.git | streaming Tiny | 3 | 4,009 | 4,444 | 28.8 | 24.5 | 59.6 | 4,847 | 6,795 |
| doorbearer.git | native Git HTTP (after) | 3 | 4,039 | 4,430 | 24.3 | — | 35.3 | 5,095 | 9,770 |
| atlas.git | streaming Tiny | 3 | 10,233 | 9,148 | 27.7 | 26.8 | 67 | 10,290 | 14,383 |
| atlas.git | native Git HTTP (after) | 3 | 9,600 | 8,918 | 26.4 | — | 36.8 | 14,813 | 12,208 |
| nzip.git | buffered Tiny | 3 | 186.7 | 100.1 | 56.7 | 33.4 | 99.4 | 113.8 | 114.8 |
| nzip.git | native Git HTTP (before) | 3 | 79.9 | 68.7 | 24.5 | — | 31.1 | 76.5 | 78.3 |
| bindws.git | buffered Tiny | 0 | FAILED: Command failed: git -C /tmp/tiny-git-benchmark-pCF5Bp push http://127.0.0.1:17447/npub1t23e93dv97z5k320gm3tjp65z6ga249tt76c2pjwlx0rxr7wwvss9tuxhn/bindws-1.git r | — | — | — | — | — | — |
| diagramzip.git | buffered Tiny | 3 | 1,578 | 1,844 | 33.8 | 27.7 | 58.8 | 1,888 | 2,391 |
| diagramzip.git | native Git HTTP (before) | 3 | 1,401 | 1,658 | 24.6 | — | 35.9 | 1,717 | 1,958 |
| strudel.git | buffered Tiny | 3 | 4,639 | 3,052 | 783.7 | 615.6 | 1,510 | 3,499 | 4,933 |
| strudel.git | native Git HTTP (before) | 3 | 2,470 | 1,935 | 32.3 | — | 56.2 | 2,616 | 3,889 |
| doorbearer.git | buffered Tiny | 3 | 4,024 | 5,669 | 33.3 | 18.6 | 60.4 | 5,876 | 7,782 |
| doorbearer.git | native Git HTTP (before) | 3 | 3,951 | 4,111 | 25.3 | — | 34.6 | 5,543 | 5,448 |
| atlas.git | buffered Tiny | 3 | 9,906 | 12,971 | 47.1 | FAILED: Nostr request timed out after 30 seconds | — | — | — |
| atlas.git | native Git HTTP (before) | 3 | 9,275 | 9,229 | 28.6 | — | — | — | — |
| atlas.git | buffered Tiny | 0 | FAILED: Command failed: git -C /tmp/tiny-git-benchmark-szM6s6 push http://127.0.0.1:17447/npub15a63zxpyhpz85v554d6f24gctr840ytc3xsaq7pk3xd076nrhacqxc79mu/atlas-0.git re | — | — | — | — | — | — |
| atlas.git | private Tiny (NIP-98) | 3 | 13,537 | 9,005 | 39.6 | 23.2 | 79 | 10,601 | 18,337 |

## Resource results

| repo | method | Go RSS high-water MiB | cgroup anon peak MiB | cgroup file peak MiB | cgroup total peak MiB | CPU initial µs | CPU incremental µs | CPU concurrent2 µs | CPU concurrent4 µs | container |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| nzip.git | streaming Tiny | 34.4 | 13.3 | 5.46 | 30.9 | 204,937 | — | — | — | tiny-git-perf-final |
| nzip.git | native Git HTTP (after) | 8.03 | 2.69 | 0.516 | 10.1 | — | — | — | — | tiny-git-perf-final-baseline |
| bindws.git | streaming Tiny | 35.3 | 23.4 | 14.8 | 57.1 | 395,910 | — | 531,919 | — | tiny-git-perf-final |
| bindws.git | native Git HTTP (after) | 10.3 | 12.6 | 9.66 | 42.7 | — | — | 469,261 | — | tiny-git-perf-final-baseline |
| diagramzip.git | streaming Tiny | 37.1 | 122.3 | 82.1 | 209.6 | 1,531,645 | — | 837,444 | 2,024,123 | tiny-git-perf-final |
| diagramzip.git | native Git HTTP (after) | 11.7 | 49.5 | 76.5 | 190.4 | 1,745,696 | — | 860,621 | 1,412,613 | tiny-git-perf-final-baseline |
| strudel.git | streaming Tiny | 39.3 | 179.2 | 174.1 | 372.7 | 6,636,176 | — | 1,604,850 | 3,353,508 | tiny-git-perf-final |
| strudel.git | native Git HTTP (after) | 12.2 | 165.0 | 167.7 | 357.4 | 6,434,735 | — | 1,323,538 | 2,209,831 | tiny-git-perf-final-baseline |
| doorbearer.git | streaming Tiny | 40.3 | 40.7 | 548.1 | 621.3 | 4,892,027 | — | 5,021,984 | 10,789,638 | tiny-git-perf-final |
| doorbearer.git | native Git HTTP (after) | 12.6 | 29.6 | 540.8 | 601.6 | 4,548,602 | — | 4,224,315 | 10,196,891 | tiny-git-perf-final-baseline |
| atlas.git | streaming Tiny | 40.6 | 179.8 | 1,477 | 1,765 | 11,991,370 | — | 11,659,980 | 28,932,729 | tiny-git-perf-final |
| atlas.git | native Git HTTP (after) | 12.8 | 178.2 | 1,469 | 1,756 | 11,375,064 | — | 11,745,735 | 27,511,063 | tiny-git-perf-final-baseline |
| nzip.git | buffered Tiny | 39.7 | 18.7 | 10.1 | 36.6 | 319,330 | — | — | 511,003 | tiny-git-perf-before |
| nzip.git | native Git HTTP (before) | 9.43 | 4.03 | 0.52 | 15.9 | — | — | — | — | tiny-git-perf-baseline |
| bindws.git | buffered Tiny | 39.9 | 18.9 | 10.2 | 36.6 | 417,278 | — | — | — | tiny-git-perf-before |
| diagramzip.git | buffered Tiny | 813.4 | 792.4 | 77.6 | 889.9 | 1,910,533 | — | 1,905,437 | 1,340,161 | tiny-git-perf-before |
| diagramzip.git | native Git HTTP (before) | 11.1 | 63.6 | 67.4 | 180.7 | 1,471,525 | — | 1,238,327 | 1,567,270 | tiny-git-perf-baseline |
| strudel.git | buffered Tiny | 1,566 | 1,566 | 170.1 | 1,766 | 8,893,943 | 1,629,815 | 4,022,310 | 9,036,055 | tiny-git-perf-before |
| strudel.git | native Git HTTP (before) | 11.7 | 108.0 | 158.6 | 340.7 | 6,261,466 | — | 1,354,373 | 3,098,503 | tiny-git-perf-baseline |
| doorbearer.git | buffered Tiny | 3,732 | 3,729 | 544.2 | 4,300 | 4,823,998 | — | 4,355,754 | 10,081,976 | tiny-git-perf-before |
| doorbearer.git | native Git HTTP (before) | 11.6 | 29.4 | 532.0 | 580.1 | 4,500,482 | — | 4,283,006 | 10,758,873 | tiny-git-perf-baseline |
| atlas.git | buffered Tiny | 6,094 | 6,097 | 1,365 | 7,256 | 11,809,027 | — | — | — | tiny-git-perf-before |
| atlas.git | native Git HTTP (before) | 10.8 | 143.4 | 1,414 | 1,663 | 11,397,964 | — | — | — | tiny-git-perf-baseline |
| atlas.git | buffered Tiny | 6,094 | 226.7 | 2,125 | 7,256 | 11,603,962 | — | — | — | tiny-git-perf-before |
| atlas.git | private Tiny (NIP-98) | 38.9 | 180.1 | 1,860 | 2,158 | 12,983,504 | — | 11,985,201 | 29,095,672 | tiny-git-perf-private |

## Probe results

| repo | method | idle publish P50 ms | idle publish P95 ms | idle publish max ms | idle query P50 ms | idle query P95 ms | idle query max ms | busy publish P50 ms | busy publish P95 ms | busy publish max ms | busy query P50 ms | busy query P95 ms | busy query max ms | idle errors | busy errors |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| nzip.git | streaming Tiny | 6.4 | 11.7 | 19.5 | 1.73 | 4.12 | 20.8 | 6.04 | 15.3 | 18.6 | 1.33 | 2.28 | 2.69 | 0 | 0 |
| nzip.git | native Git HTTP (after) | — | — | — | — | — | — | 7.44 | 10.7 | 10.8 | 1.16 | 1.55 | 1.65 | 0 | 0 |
| bindws.git | streaming Tiny | 6.11 | 7.38 | 10.3 | 1.63 | 2.05 | 5.05 | 5.88 | 12.8 | 23 | 1.43 | 1.92 | 5.43 | 0 | 0 |
| bindws.git | native Git HTTP (after) | — | — | — | — | — | — | 5.96 | 17.8 | 20 | 1.32 | 1.72 | 2.33 | 0 | 0 |
| diagramzip.git | streaming Tiny | 6.16 | 10.3 | 12 | 1.56 | 2.05 | 4.81 | 5.7 | 17.3 | 102.2 | 1.33 | 1.73 | 3.13 | 0 | 0 |
| diagramzip.git | native Git HTTP (after) | — | — | — | — | — | — | 5.81 | 17.6 | 84.7 | 1.33 | 1.7 | 2.24 | 0 | 0 |
| strudel.git | streaming Tiny | 6.1 | 10.2 | 15.2 | 1.54 | 1.98 | 4.3 | 6.08 | 12.7 | 330.9 | 1.36 | 2.47 | 5.88 | 0 | 0 |
| strudel.git | native Git HTTP (after) | — | — | — | — | — | — | 5.61 | 9.49 | 100.3 | 1.35 | 2.03 | 5.91 | 0 | 0 |
| doorbearer.git | streaming Tiny | 4.16 | 6.79 | 9.06 | 1.52 | 1.88 | 3.21 | 5.64 | 47.1 | 299.4 | 1.25 | 1.68 | 3.49 | 0 | 0 |
| doorbearer.git | native Git HTTP (after) | — | — | — | — | — | — | 5.83 | 47.5 | 1,117 | 1.23 | 1.62 | 3.7 | 0 | 0 |
| atlas.git | streaming Tiny | 5.86 | 10.5 | 26.2 | 1.47 | 1.9 | 5.18 | 5.64 | 68.3 | 719.3 | 1.26 | 1.71 | 331.1 | 0 | 0 |
| atlas.git | native Git HTTP (after) | — | — | — | — | — | — | 5.71 | 53.7 | 656.4 | 1.28 | 1.69 | 4.51 | 0 | 0 |
| nzip.git | buffered Tiny | 6.29 | 9.96 | 11 | 1.64 | 2.26 | 5.71 | 6.11 | 14.2 | 35.6 | 1.38 | 1.63 | 5.34 | 0 | 0 |
| nzip.git | native Git HTTP (before) | — | — | — | — | — | — | 6.26 | 7.43 | 7.57 | 1.28 | 2.82 | 3.84 | 0 | 0 |
| bindws.git | buffered Tiny | 6.27 | 11.1 | 15.7 | 1.63 | 2.25 | 21.7 | 5.79 | 6.13 | 6.18 | 1.45 | 4.77 | 5.36 | 0 | 0 |
| diagramzip.git | buffered Tiny | 6.39 | 10.7 | 12.6 | 1.71 | 2.33 | 2.52 | 6.2 | 18.6 | 201.0 | 1.34 | 1.66 | 2.13 | 0 | 0 |
| diagramzip.git | native Git HTTP (before) | — | — | — | — | — | — | 6.56 | 23.4 | 209.8 | 1.35 | 1.71 | 3.8 | 0 | 0 |
| strudel.git | buffered Tiny | 4.37 | 6.34 | 8.87 | 1.58 | 2.09 | 3.7 | 3.76 | 9.38 | 567.1 | 1.26 | 1.91 | 24.2 | 0 | 0 |
| strudel.git | native Git HTTP (before) | — | — | — | — | — | — | 4.04 | 9.99 | 179.1 | 1.3 | 2.62 | 8.03 | 0 | 0 |
| doorbearer.git | buffered Tiny | 5.9 | 9.14 | 12.2 | 1.45 | 1.8 | 2.11 | 5.72 | 46.3 | 426.0 | 1.2 | 1.59 | 2.77 | 0 | 0 |
| doorbearer.git | native Git HTTP (before) | — | — | — | — | — | — | 5.58 | 23.1 | 288.1 | 1.25 | 1.63 | 5.11 | 0 | 0 |
| atlas.git | buffered Tiny | 5.86 | 9.99 | 13.9 | 1.51 | 1.78 | 3.48 | 5.28 | 11.5 | 1,323 | 1.41 | 2.06 | 3.72 | 0 | 0 |
| atlas.git | native Git HTTP (before) | — | — | — | — | — | — | 5.53 | 19.2 | 242.5 | 1.26 | 1.72 | 4.38 | 0 | 0 |
| atlas.git | buffered Tiny | 4.58 | 6.84 | 9.25 | 1.94 | 2.41 | 6.25 | 3.43 | 6 | 91.4 | 1.1 | 1.58 | 2.14 | 0 | 0 |
| atlas.git | private Tiny (NIP-98) | 6.65 | 9.31 | 12.8 | 1.83 | 2.28 | 5.93 | 5.95 | 86.7 | 1,384 | 1.35 | 2.98 | 33.5 | 0 | 0 |

## Status

| repo | method | status | failed phases |
| --- | --- | --- | --- |
| nzip.git | streaming Tiny | complete | — |
| bindws.git | streaming Tiny | complete | — |
| diagramzip.git | streaming Tiny | complete | — |
| strudel.git | streaming Tiny | complete | — |
| doorbearer.git | streaming Tiny | complete | — |
| atlas.git | streaming Tiny | complete | — |
| nzip.git | buffered Tiny | complete | — |
| bindws.git | buffered Tiny | failed | initial_push |
| diagramzip.git | buffered Tiny | complete | — |
| strudel.git | buffered Tiny | complete | — |
| doorbearer.git | buffered Tiny | complete | — |
| atlas.git | buffered Tiny | failed | incremental_state_ack |
| atlas.git | buffered Tiny | failed | initial_push |
| atlas.git | private Tiny (NIP-98) | complete | — |

Profile finding from the captured before run: CPU samples identify memory clearing/copying as the visible hot path; the independent Go RSS reading is 6093.9 MiB. Keep the raw profile and process/cgroup captures beside any interpretation.

Raw JSON artifacts are linked from the HTML report.
