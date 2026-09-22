#!/bin/bash
# Retry the MMS snapshot download until it completes; the Hub rate-limits
# unauthenticated IPs per 300 s window. HF_HOME and the interpreter are the
# q2e ones; the prefetch script is the one next to this file.
HERE=$(cd "$(dirname "$0")" && pwd)
PY=${MMS_PYTHON:-$HOME/rfe-pilot/bakeoff-venv/bin/python}
export HF_HOME=${HF_HOME:-/mnt/data/rfe-val/hf}
i=0
while [ "$i" -lt 12 ]; do
  i=$((i + 1))
  if nice -n 10 "$PY" "$HERE/mms_prefetch.py"; then
    echo "PREFETCH OK"
    exit 0
  fi
  echo "attempt $i failed; sleeping 150"
  sleep 150
done
echo "PREFETCH FAILED"
exit 1
