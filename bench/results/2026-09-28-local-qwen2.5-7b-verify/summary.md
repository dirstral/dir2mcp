Run 2026-09-28T17:25:18Z: dir2mcp `dir2mcp v0.11.3-0.20260928164656-2f9f9c7b8273`, commit `2f9f9c7b8273244d40d05ad0a39ef32f14003b38`.
Questions: 80 answerable, 20 unanswerable in corpus, 20 unanswerable off corpus. Tool errors: 0.
Index time: 7.7 s. Ask time: 2081.6 s. Total: 2100.7 s. ask k: server default.

| Metric | Value |
| --- | --- |
| (a) Answer contains a gold answer | 67.5% (54/80) |
| (a) Mean gold-token recall | 0.736 |
| (a) Mean SQuAD token F1 (long answers score low) | 0.251 |
| (b) Inline citation precision, span level | 60.7% (37/61) |
| (b) Inline citation precision, file level | 100.0% (61/61) |
| (b) Mean inline citations for each answer | 0.762 |
| (c) Answers with a supporting inline citation, span level | 46.2% (37/80) |
| (c) Answers with a supporting inline citation, file level | 76.2% (61/80) |
| (b) `citations[]` precision, span level | 8.9% (84/939) |
| (b) `citations[]` precision, file level | 49.8% (468/939) |
| (b) Mean `citations[]` entries for each answer | 11.738 |
| (c) Answers with a supporting `citations[]` entry, span level | 78.8% (63/80) |
| (c) Answers with a supporting `citations[]` entry, file level | 78.8% (63/80) |
| (d) Abstention, all unanswerable | 75.0% (30/40) |
| (d) Abstention, unanswerable in corpus (SQuAD 2.0 adversarial) | 55.0% (11/20) |
| (d) Abstention, unanswerable off corpus | 95.0% (19/20) |
| (d) False abstention, answerable | 22.5% (18/80) |
| (e) Latency p50 / p95 / max (ms, n=120) | 16678 / 22444 / 36045 |
| Answerable questions with no citation | 17 |
| Answers that dir2mcp withheld (`evidence` = insufficient) | 0 |
| Answers that dir2mcp withheld (`faithfulness` = unsupported) | 47 |
