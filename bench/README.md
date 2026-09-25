# End-to-end answer and citation benchmark

This benchmark measures what a client gets back from `dir2mcp_ask`. It starts
from an empty state dir. It indexes a public corpus, asks each question over
MCP, and scores the answer text and the citations against gold data.

It is not the same as `tests/eval/`. That suite is an in-process retrieval
ablation (recall@k and nDCG over fixtures). This benchmark runs the real binary
and the real models, and it scores the final answer.

## Run it

You need Python 3 (no third-party packages; tested with 3.14), Go (to build
the binary), and the providers in the config file. The default config
(`bench/config.local.yaml`) uses a local Ollama on its default port,
`http://127.0.0.1:11434`, with the two models in the Results section.

```bash
make bench-e2e                                     # or: make bench-e2e BENCH_CONFIG=my.yaml
```

This command builds `./dir2mcp`, then runs `bench/run.py`. Other forms:

```bash
python3 bench/run.py --config my-providers.yaml   # other models
python3 bench/run.py --limit 5                    # quick check with 5 questions
python3 bench/score.py                            # score bench/work/results.json again
```

The runner writes into `bench/work/` (git ignores it):

| File | Content |
| --- | --- |
| `cache/dev-v2.0.json` | The dataset, verified against the sha256 in `manifest.json` |
| `corpus/*.md` | The corpus that dir2mcp indexes |
| `questions.jsonl` | The question set with gold data |
| `state/`, `home/`, `up.log` | The daemon state, its empty HOME, and its log |
| `results.json` | For each question: answer, `citations[]`, latency, errors |
| `scores.json`, `summary.md` | The scores |

The daemon gets a clean environment: an empty HOME, PATH and TERM only. No
provider key from your shell reaches it.

## Dataset and license

The data is the development set of SQuAD 2.0 (Rajpurkar, Jia and Liang, 2018,
"Know What You Don't Know: Unanswerable Questions for SQuAD").

- Source: https://rajpurkar.github.io/SQuAD-explorer/dataset/dev-v2.0.json
- sha256: `80a5225e94905956a6446d296ca1093975c4d3b3260f1d6c8f68bc2ab77182d8`
- License: [CC BY-SA 4.0](https://creativecommons.org/licenses/by-sa/4.0/),
  as the SQuAD site states. The paragraphs come from English Wikipedia.

The repository does not hold the dataset. `bench/prepare.py` downloads it and
stops if the checksum does not match. The files in `bench/results/` hold
question text and answers derived from SQuAD 2.0, so they are under
CC BY-SA 4.0 too.

### How prepare.py builds the benchmark

- Corpus: 10 articles (see `corpus_articles` in `manifest.json`). Each article
  becomes one Markdown file. Line 1 is the title. Each paragraph is one line,
  and a blank line separates paragraphs. Thus each SQuAD paragraph has one line
  number, and that line is the gold span.
- Answerable questions: 8 for each corpus article (80). Each has the SQuAD gold
  answers, the gold file, and the gold line.
- Unanswerable in corpus: 2 for each corpus article (20). These are the SQuAD
  2.0 questions that the annotators wrote to look answerable from the
  paragraph, but that it does not answer. They are adversarial.
- Unanswerable off corpus: 5 for each of 4 articles that are not in the corpus
  (20). prepare.py drops a question when one of its gold answers occurs in the
  corpus text, so the corpus cannot answer it by accident.
- Selection: prepare.py sorts the questions by the sha256 of their SQuAD id,
  and it takes the first N of each group. The same pin gives the same 120 questions on each
  machine.

Limits of this dataset: SQuAD questions were written for one paragraph. Some
are vague without that paragraph ("Who sold the rights?"). Here the system
must first find the paragraph in a 10-article corpus. Also, the gold line is
the one paragraph that SQuAD marks. Another paragraph can state the same fact,
so the span-level citation scores are a lower bound.

## Metrics

All rules are deterministic. No model judges an answer. `bench/score.py`
holds the code, and `bench/test_score.py` tests it. "Normalized" means the
SQuAD rule: lower case, remove punctuation, remove the articles a, an, the,
and collapse white space. One change: the SQuAD script removes only ASCII
punctuation, and this one also removes Unicode punctuation (curly quotes, en
dashes).

**(a) Answer correctness** (answerable questions only):

- *Answer contains a gold answer* (the main number): the normalized answer
  holds one normalized gold answer as a sequence of whole tokens. A tool error
  counts as wrong.
- *Mean gold-token recall*: for each question, the best share of gold-answer
  tokens that occur in the answer. Then the mean.
- *Mean SQuAD token F1*: the standard SQuAD F1, best over the gold answers.
  dir2mcp writes full sentences with citation tags, so the precision half of F1
  is low. We show it for comparison with SQuAD papers only.

**(b) Citation precision** and **(c) supporting-citation rate** (answerable
questions only). We score two sets of citations:

- *Inline citations*: the bracketed tags that the model writes in the answer
  text, for example `[Normans.md:L53-L63]`. The `Sources:` footer that dir2mcp
  adds is not counted, because it repeats the inline tags.
- *`citations[]`*: the structured list in `structuredContent`. dir2mcp puts
  each passage that it gave to the model in this list, so it is the retrieved
  context, not the claims of the answer.

A citation *supports* the answer at **file level** when its `rel_path` is the
gold file. It supports at **span level** when it is also a line span, and the
range `start_line..end_line` includes the gold line.

- Precision = supporting citations / all citations, over all answerable
  questions (micro average).
- Supporting-citation rate = share of answerable questions with at least one
  supporting citation. This is the citation recall for a single gold passage.

**(d) Abstention.** An answer abstains when it matches one of the patterns in
`ABSTAIN_PATTERNS` in `score.py`. The patterns include the two fixed texts that
dir2mcp returns when it does not answer ("Insufficient evidence to answer",
"No relevant context found"), and the usual model phrases ("does not
contain", "is not mentioned", "no information", "cannot determine", and
similar).

- Abstention = share of unanswerable questions whose answer abstains. We show
  it for each group and for both groups together. Higher is better.
- False abstention = share of answerable questions whose answer abstains.
  Lower is better.

The phrase rule is a heuristic. An answer that gives a fact and also says
"the context does not specify the exact year" counts as an abstention.

**(e) Latency.** The wall time of each `dir2mcp_ask` call, from the request
to the full response, over all 120 questions. p50 and p95 use the
nearest-rank method.

## Results

One full run on 2026-09-24. The raw files are in
[`results/2026-09-24-local-qwen2.5-7b/`](results/2026-09-24-local-qwen2.5-7b/).

Setup:

- Binary: built with `make build` from commit
  `60f9b9f95be82dc34cb7db2638afdc434297d4e1` (main). `dir2mcp version` prints
  `dir2mcp v0.11.2-0.20260924161426-60f9b9f95be8`. This PR does not change
  product code.
- Config: `bench/config.local.yaml`, with `base_url` set to port 21434 (the
  local end of the SSH tunnel). All other settings are the server defaults. Each ask used the server default k and got 14 or 15 passages.
- Embeddings: `nomic-embed-text:latest` (137M, F16). Chat:
  `qwen2.5:7b-instruct-q4_K_M` (7.6B, Q4_K_M). Both on Ollama 0.33.2.
- Hardware: dir2mcp and the runner ran on an Apple M4 Mac with 16 GB (macOS,
  arm64, Python 3.14.7). Ollama ran on a remote Linux x86_64 host with an
  NVIDIA L4 (24 GB vGPU profile), through an SSH tunnel. The latency includes
  the tunnel.
- Duration: index 7.8 s (10 files, 94 chunks, 0 errors), 120 asks 1397 s,
  daemon start to stop 1417 s (about 24 minutes).

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

What the numbers say:

- **Answers.** 75% of the answers hold a gold answer. We read the 20 misses.
  About 6 are correct paraphrases that the strict rule rejects, for example
  "declared martial law" against the gold "declare martial law". The mean
  gold-token recall (0.837) shows this effect. 3 are false abstentions. 1
  answer is only a citation tag, with no text. The others are wrong answers,
  for example "between Koblenz and Bonn" (gold: "Rüdesheim am Rhein").
- **Retrieval.** For each of the 80 answerable questions, the gold paragraph
  was in `citations[]`. In this small corpus the retriever always found the
  passage. The low `citations[]` precision is by design: dir2mcp lists every
  passage that it gave to the model, and each list has about 15 entries.
- **Inline citations.** The model wrote about one tag for each answer. The tag
  named the correct file in 98.7% of cases. The line range held the gold
  paragraph in only 67.1% of cases. 67 of the 79 tags copy a header tag
  exactly; the other 12 have line numbers that the model changed or made up
  (for example `[Normans.md:L32-33]` for a passage that the header tags
  `[Normans.md:L25-L33]`).
- **Abstention is the weak point.** The model abstained on 45% of the
  off-corpus questions and on 5% of the adversarial SQuAD 2.0 questions. It
  answered the rest from its own knowledge or from a wrong passage, and it
  often attached a citation to an unrelated file. The dir2mcp
  insufficient-evidence guard (SPEC §9.4.3) did not withhold any answer: all
  120 answers carry `evidence: sufficient`, also the 20 off-corpus ones.
  With this embedder, the cosine of an unrelated passage is above the shipped
  threshold (0.05).
- **Repeatability.** A first run on the same day, with the same binary and the
  same questions, gave 117 of 120 identical answers. Its scores: 76.2%
  (61/80) contains-gold, 65.8% inline span precision, the same abstention
  numbers, latency p50 11.06 s and p95 16.98 s. That run is not committed,
  because its runner did not yet record the `evidence` field.

Do not compare these numbers with SQuAD leaderboard numbers. SQuAD gives the
model the gold paragraph. Here the system must find it in the corpus, and it
writes a free-form answer with citations.
