#!/usr/bin/env python3
"""Download facebook/mms-1b-all once, with only the adapters the rig uses.

Run under the torch venv with HF_HOME pointed at a big disk:
  HF_HOME=/mnt/data/rfe-val/hf python mms_prefetch.py
The pattern list is exact filenames on purpose: "*.safetensors" matches the
1100+ per-language adapters (about 9 MB each) and trips the Hub's
unauthenticated rate limit. mms_prefetch_loop.sh retries across that limit.
"""
import glob
import os

os.environ.setdefault("HF_HOME", "/mnt/data/rfe-val/hf")
from huggingface_hub import snapshot_download  # noqa: E402

FILES = [
    "config.json", "preprocessor_config.json", "tokenizer_config.json", "vocab.json",
    "special_tokens_map.json", "model.safetensors",
    "adapter.kir.safetensors", "adapter.ukr.safetensors", "adapter.rus.safetensors",
    "adapter.kat.safetensors",
]

if __name__ == "__main__":
    path = snapshot_download("facebook/mms-1b-all", allow_patterns=FILES)
    print("snapshot", path)
    for f in sorted(glob.glob(path + "/*")):
        print(os.path.getsize(f), os.path.basename(f))
