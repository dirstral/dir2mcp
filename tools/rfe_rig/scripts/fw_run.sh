#!/bin/bash
# Sequential CPU decode of the three Kyrgyz recordings with the #566
# specialist (UlutSoftLLC/whisper-small-kyrgyz as CTranslate2), int8, 4 threads.
# language=kk: whisper's tokenizer has no Kyrgyz slot, so the fine-tune was
# trained to emit under the Kazakh token (the pilot's ky_pipeline.py does the same).
set -u
cd /mnt/data/rfe-val/rig || exit 1
R=/mnt/data/rfe-val/rig/results/2026-09-22
C=/mnt/data/rfe-val/corpus_live
PY=/home/ubuntu/rfe-pilot/stt-venv/bin/python
M=/home/ubuntu/rfe-pilot/models/small-kyrgyz-ct2
for f in kgz_29216_rahat_interview.wav kgz_18448_extremism_hospital.flac kgz_21138_japarov_broll.flac; do
  echo "=== $(date -u +%FT%TZ) fw small-kyrgyz $f"
  nice -n 10 python3 -m tools.rfe_rig --results "$R" transcribe \
    --decoder "fw:$M?language=kk&name=whisper-small-kyrgyz-ct2" --fw-python "$PY" --threads 4 "$C/$f"
  rc=$?
  echo "=== $(date -u +%FT%TZ) exit $rc"
done
echo "FW BATCH DONE $(date -u +%FT%TZ)"
