"""Shared helpers for the sr-mesh scripts: talking to polad via the `pola`
CLI, resolving PCEP sessions to TED router IDs, and computing the desired
full-mesh of dynamic SR policies.

Deliberately stdlib-only so it can run on a lab host without a Python
package install step.
"""
from __future__ import annotations

import ipaddress
import json
import os
import shutil
import subprocess
import tempfile
from dataclasses import dataclass
from pathlib import Path


def resolve_pola_bin(pola_bin: str) -> str:
    """Resolve the pola binary, preferring PATH but falling back to the
    conventional `go install` location used in this lab."""
    if shutil.which(pola_bin):
        return pola_bin
    fallback = Path.home() / "go" / "bin" / "pola"
    if fallback.exists():
        return str(fallback)
    return pola_bin  # let subprocess raise a clear "not found" error


def sanitize_name(value: str) -> str:
    return value.replace(".", "-").replace(":", "-").strip("-")


def node_label(node: dict) -> str:
    hostname = (node.get("hostname") or "").strip()
    return sanitize_name(hostname) if hostname else sanitize_name(node["routerID"])


def node_is_sr_capable(node: dict) -> bool:
    """A node is SR-capable if it advertises a valid SRGB and at least one
    prefix carries a Prefix-SID (sidIndex present)."""
    if not node.get("srgbBegin") or not node.get("srgbEnd"):
        return False
    return any("sidIndex" in prefix and prefix["sidIndex"] is not None for prefix in node.get("prefixes", []))


def find_node_by_addr(ted_nodes: list[dict], addr: str) -> dict | None:
    """Match a session's peer address against each node's /32 (or /128)
    loopback prefix."""
    try:
        target = ipaddress.ip_address(addr)
    except ValueError:
        return None
    for node in ted_nodes:
        for prefix in node.get("prefixes", []):
            try:
                network = ipaddress.ip_network(prefix["prefix"], strict=False)
            except (ValueError, KeyError):
                continue
            if network.num_addresses == 1 and network.network_address == target:
                return node
    return None


def valid_sessions(sessions: list[dict]) -> list[dict]:
    return [s for s in sessions if s.get("State") == "SESSION_STATE_UP" and s.get("IsSynced") is True]


def sr_capable_nodes(ted_nodes: list[dict]) -> list[dict]:
    return [n for n in ted_nodes if node_is_sr_capable(n)]


def resolve_session_nodes(sessions: list[dict], ted_nodes: list[dict], log) -> tuple[list[tuple[dict, dict]], list[str]]:
    """Resolve each session's peer address to its TED node. Returns
    (resolved [(session, node), ...], unresolved [addr, ...])."""
    resolved = []
    unresolved = []
    for session in sessions:
        addr = session["Addr"]
        node = find_node_by_addr(ted_nodes, addr)
        if node is None:
            unresolved.append(addr)
            log.warning("session %s could not be resolved to a TED router ID, skipping as mesh endpoint", addr)
            continue
        resolved.append((session, node))
    return resolved, unresolved


@dataclass(frozen=True)
class MeshPolicy:
    session_addr: str
    src_router_id: str
    dst_router_id: str
    name: str
    asn: int
    color: int
    metric: str

    def yaml(self) -> str:
        return (
            f"asn: {self.asn}\n"
            "srPolicy:\n"
            f"  pcepSessionAddr: {self.session_addr}\n"
            f"  name: {self.name}\n"
            f"  srcRouterID: {self.src_router_id}\n"
            f"  dstRouterID: {self.dst_router_id}\n"
            f"  color: {self.color}\n"
            "  type: dynamic\n"
            f"  metric: {self.metric}\n"
        )


def build_desired_mesh(
    resolved_sessions: list[tuple[dict, dict]],
    dst_nodes: list[dict],
    asn: int,
    color: int,
    metric: str,
    name_prefix: str,
) -> list[MeshPolicy]:
    """Every ordered (session, destination) pair, excluding self-policies."""
    policies = []
    for session, src_node in resolved_sessions:
        src_label = node_label(src_node)
        for dst_node in dst_nodes:
            if dst_node["routerID"] == src_node["routerID"]:
                continue
            name = f"{name_prefix}-{src_label}-{node_label(dst_node)}"
            policies.append(
                MeshPolicy(
                    session_addr=session["Addr"],
                    src_router_id=src_node["routerID"],
                    dst_router_id=dst_node["routerID"],
                    name=name,
                    asn=asn,
                    color=color,
                    metric=metric,
                )
            )
    return policies


def extract_error_message(stderr: str) -> str:
    """pola's cobra-based errors print an optional usage block followed by
    an `Error: ...` line; pull just the message out of the noise."""
    for line in stderr.splitlines():
        if line.startswith("Error:"):
            return line[len("Error:"):].strip()
    return stderr.strip()


@dataclass
class AddResult:
    ok: bool
    stdout: str
    stderr: str
    returncode: int

    @property
    def reason(self) -> str:
        return extract_error_message(self.stderr) or self.stdout or f"exit code {self.returncode}"


class PolaCLIError(RuntimeError):
    def __init__(self, what: str, proc: subprocess.CompletedProcess):
        message = extract_error_message(proc.stderr) or proc.stdout.strip() or f"exit code {proc.returncode}"
        super().__init__(f"`pola {what} -j` failed: {message}")
        self.proc = proc


class PolaCLI:
    def __init__(self, pola_bin: str, host: str, port: str):
        self.pola_bin = pola_bin
        self.host = host
        self.port = str(port)

    def _run(self, args: list[str]) -> subprocess.CompletedProcess:
        cmd = [self.pola_bin, "--host", self.host, "--port", self.port, *args]
        try:
            return subprocess.run(cmd, capture_output=True, text=True)
        except OSError as e:
            # e.g. the pola binary isn't on PATH / isn't executable. Degrade to
            # a synthetic failed CompletedProcess so every caller's existing
            # returncode/stderr handling covers this without special-casing it.
            return subprocess.CompletedProcess(
                cmd, returncode=127, stdout="", stderr=f"Error: could not execute '{self.pola_bin}': {e}"
            )

    def _run_json(self, args: list[str]) -> tuple[object | None, subprocess.CompletedProcess]:
        proc = self._run([*args, "-j"])
        stdout = proc.stdout.strip()
        if proc.returncode != 0 or not stdout:
            return None, proc
        try:
            return json.loads(stdout), proc
        except json.JSONDecodeError:
            return None, proc

    def sessions(self) -> list[dict]:
        data, proc = self._run_json(["session"])
        if data is None:
            raise PolaCLIError("session", proc)
        return data

    def ted(self) -> list[dict]:
        data, proc = self._run_json(["ted"])
        if data is None:
            raise PolaCLIError("ted", proc)
        return data.get("ted") or []

    def sr_policy_list(self, session: str | None = None) -> list[dict]:
        args = ["sr-policy", "list"]
        if session:
            args += ["--session", session]
        data, proc = self._run_json(args)
        if data is None:
            raise PolaCLIError("sr-policy list", proc)
        return data

    def sr_policy_add(self, yaml_text: str) -> AddResult:
        fd, path = tempfile.mkstemp(suffix=".yaml", prefix="sr-mesh-")
        try:
            with os.fdopen(fd, "w") as f:
                f.write(yaml_text)
            proc = self._run(["sr-policy", "add", "-f", path, "-j"])
        finally:
            try:
                os.unlink(path)
            except OSError:
                pass
        ok = proc.returncode == 0 and '"status": "success"' in proc.stdout
        return AddResult(ok=ok, stdout=proc.stdout.strip(), stderr=proc.stderr.strip(), returncode=proc.returncode)
