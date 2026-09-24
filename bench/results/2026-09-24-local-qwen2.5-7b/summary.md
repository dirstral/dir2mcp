Run 2026-09-24T22:54:50Z: dir2mcp `dir2mcp v0.11.2-0.20260924161426-60f9b9f95be8`, commit `60f9b9f95be82dc34cb7db2638afdc434297d4e1`.
Questions: 80 answerable, 20 unanswerable in corpus, 20 unanswerable off corpus. Tool errors: 0.
Index time: 7.8 s. Ask time: 1397.4 s. Total: 1416.6 s. ask k: server default.

| Metric | Value |
| --- | --- |
| (a) Answer contains a gold answer | 75.0% (60/80) |
| (a) Mean gold-token recall | 0.837 |
| (a) Mean SQuAD token F1 (long answers score low) | 0.266 |
| (b) Inline citation precision, span level | 67.1% (53/79) |
| (b) Inline citation precision, file level | 98.7% (78/79) |
| (b) Mean inline citations for each answer | 0.988 |
| (c) Answers with a supporting inline citation, span level | 65.0% (52/80) |
| (c) Answers with a supporting inline citation, file level | 96.2% (77/80) |
| (b) `citations[]` precision, span level | 8.9% (106/1190) |
| (b) `citations[]` precision, file level | 50.3% (599/1190) |
| (b) Mean `citations[]` entries for each answer | 14.875 |
| (c) Answers with a supporting `citations[]` entry, span level | 100.0% (80/80) |
| (c) Answers with a supporting `citations[]` entry, file level | 100.0% (80/80) |
| (d) Abstention, all unanswerable | 25.0% (10/40) |
| (d) Abstention, unanswerable in corpus (SQuAD 2.0 adversarial) | 5.0% (1/20) |
| (d) Abstention, unanswerable off corpus | 45.0% (9/20) |
| (d) False abstention, answerable | 6.2% (5/80) |
| (e) Latency p50 / p95 / max (ms, n=120) | 10881 / 17017 / 23699 |
| Answerable questions with no citation | 0 |
| Answers that dir2mcp withheld (`evidence` = insufficient) | 0 |
