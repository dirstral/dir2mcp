#!/bin/bash
# Sequential CPU decode of the RFE corpus with facebook/mms-1b-all.
# Adapter follows the speech language, not the file prefix: the geo_ and
# ukr_1108 recordings are Russian speech, kuleba mixes English and Ukrainian.
set -u
cd /mnt/data/rfe-val/rig || exit 1
export HF_HOME=/mnt/data/rfe-val/hf HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1
R=/mnt/data/rfe-val/rig/results/2026-09-22
C=/mnt/data/rfe-val/corpus_live
PY=/home/ubuntu/rfe-pilot/bakeoff-venv/bin/python
failed=0
run() {
  adapter=$1; shift
  echo "=== $(date -u +%FT%TZ) mms:$adapter $*"
  nice -n 10 python3 -m tools.rfe_rig --results "$R" transcribe --decoder "mms:$adapter" \
    --mms-python "$PY" --threads 10 "$@"
  rc=$?
  echo "=== $(date -u +%FT%TZ) exit $rc"
  if [ "$rc" -ne 0 ]; then failed=$((failed + 1)); fi
  return "$rc"
}
run kir "$C/kgz_29216_rahat_interview.wav"
run kir "$C/kgz_18448_extremism_hospital.flac"
run ukr "$C/ukr_0652_filaret_programme.flac"
run rus "$C/rus_28974_shalygina_interview.flac"
run kir "$C/kgz_21138_japarov_broll.flac"
run ukr "$C/ukr_18126_kuleba_interview.flac"
run rus "$C/geo_2007_kikabidze_interview.flac"
run rus "$C/ukr_1108_kravchuk_interview.flac"
if [ "$failed" -ne 0 ]; then
  echo "MMS BATCH INCOMPLETE: $failed decode(s) failed $(date -u +%FT%TZ)"
  exit 1
fi
echo "MMS BATCH DONE $(date -u +%FT%TZ)"
