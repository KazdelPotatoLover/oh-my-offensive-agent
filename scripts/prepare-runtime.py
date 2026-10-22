#!/usr/bin/env python3
"""Prepare a local broker credential without replacing existing model settings."""
from pathlib import Path
import secrets
import subprocess

root = Path(__file__).resolve().parents[1]
env = root / '.env'
if not env.exists():
    # A worktree can reuse the main checkout's local model configuration.
    common = Path(subprocess.check_output(['git', 'rev-parse', '--git-common-dir'], cwd=root, text=True).strip())
    if not common.is_absolute():
        common = root / common
    original = common.resolve().parent / '.env'
    source = original if original.exists() else root / '.env.example'
    env.touch(mode=0o600)
    env.write_text(source.read_text())
text = env.read_text()
configured = any(line.strip().startswith('OMA_BROKER_TOKEN=') and line.split('=', 1)[1].strip().strip("\"'") for line in text.splitlines())
if not configured:
    lines = [line for line in text.splitlines() if not line.strip().startswith('OMA_BROKER_TOKEN=')]
    env.write_text('\n'.join(lines) + '\nOMA_BROKER_TOKEN=' + secrets.token_hex(32) + '\n')
    env.chmod(0o600)
print('Local runtime configuration ready; existing model settings preserved.')
