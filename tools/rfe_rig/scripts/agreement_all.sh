#!/bin/bash
# Every decoder pair scored in the 2026-09-22 baseline. A = dir2mcp's delivered
# whisper large-v3-multi transcript (the sqlite: decoder) unless stated.
# Usage: agreement_all.sh RESULTS_DIR CORPUS_DIR
set -u
R=${1:?results dir}
C=${2:?corpus dir}
cd "$(dirname "$0")/../../.." || exit 1
pair() {
  echo "=== $1  A=$2  B=$3"
  python3 -m tools.rfe_rig --results "$R" agreement --recording "$C/$1" --a "$2" --b "$3" | head -12
}
# Russian speech: whisper vs GigaAM (GPU server) and vs MMS rus (CPU)
for f in rus_28974_shalygina_interview.flac geo_2007_kikabidze_interview.flac ukr_1108_kravchuk_interview.flac; do
  pair "$f" dir2mcp- gigaam
  pair "$f" dir2mcp- mms-1b-all-rus
  pair "$f" gigaam mms-1b-all-rus
done
# Kyrgyz speech: whisper vs MMS kir, whisper vs the #566 specialist, specialist vs MMS
for f in kgz_29216_rahat_interview.wav kgz_18448_extremism_hospital.flac kgz_21138_japarov_broll.flac; do
  pair "$f" dir2mcp- mms-1b-all-kir
  pair "$f" dir2mcp- whisper-small-kyrgyz
  pair "$f" whisper-small-kyrgyz mms-1b-all-kir
done
# Ukrainian speech (kuleba mixes English): whisper vs MMS ukr
for f in ukr_0652_filaret_programme.flac ukr_18126_kuleba_interview.flac; do
  pair "$f" dir2mcp- mms-1b-all-ukr
done
