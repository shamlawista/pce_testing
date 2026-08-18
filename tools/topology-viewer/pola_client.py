"""Minimal `pola` CLI wrapper: shells out for `ted -j`, `session -j`, and
`sr-policy list -j`. Self-contained (not shared with tools/sr-mesh) so this
tool has no cross-directory coupling to a differently-purposed script.
"""
from __future__ import annotations

import json
import shutil
import subprocess
from pathlib import Path


class PolaCLIError(RuntimeError):
    pass


def resolve_pola_bin(pola_bin: str) -> str:
    if shutil.which(pola_bin):
        return pola_bin
    fallback = Path.home() / "go" / "bin" / "pola"
    if fallback.exists():
        return str(fallback)
    return pola_bin  # let subprocess raise a clear "not found" error


class PolaClient:
    def __init__(self, pola_bin: str, host: str, port: str):
        self.pola_bin = resolve_pola_bin(pola_bin)
        self.host = host
        self.port = str(port)

    def _run_json(self, args: list[str]):
        cmd = [self.pola_bin, "--host", self.host, "--port", self.port, *args, "-j"]
        try:
            proc = subprocess.run(cmd, capture_output=True, text=True, timeout=15)
        except OSError as e:
            raise PolaCLIError(f"could not execute '{self.pola_bin}': {e}") from e
        except subprocess.TimeoutExpired as e:
            raise PolaCLIError(f"pola {' '.join(args)} timed out: {e}") from e

        stdout = proc.stdout.strip()
        if proc.returncode != 0:
            reason = proc.stderr.strip() or stdout or f"exit code {proc.returncode}"
            raise PolaCLIError(f"pola {' '.join(args)} failed: {reason}")
        try:
            return json.loads(stdout)
        except json.JSONDecodeError as e:
            raise PolaCLIError(f"pola {' '.join(args)} returned unparsable JSON: {e}\noutput: {stdout!r}") from e

    def ted(self) -> list[dict]:
        data = self._run_json(["ted"])
        return (data or {}).get("ted") or []

    def sessions(self) -> list[dict]:
        return self._run_json(["session"]) or []

    def sr_policy_list(self) -> list[dict]:
        return self._run_json(["sr-policy", "list"]) or []
