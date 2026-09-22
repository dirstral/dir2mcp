#!/bin/bash
# Retry the MMS snapshot download until it completes; the Hub rate-limits
# unauthenticated IPs per 300 s window.
cd /mnt/data/rfe-val/rig || exit 1
i=0
while [ "$i" -lt 12 ]; do
  i=$((i + 1))
  if HF_HOME=/mnt/data/rfe-val/hf nice -n 10 ~/rfe-pilot/bakeoff-venv/bin/python mms_prefetch.py; then
    echo "PREFETCH OK"
    exit 0
  fi
  echo "attempt $i failed; sleeping 150"
  sleep 150
done
echo "PREFETCH FAILED"
exit 1
