#!/usr/bin/env python3
"""Reproducible benchmark harness for the Go FLC pilot.

Runs each experiment as an isolated child `goflc` process (cold and warm), parses each run-manifest.json, and emits
artifacts/benchmarks/benchmark.json plus a Markdown summary. Does NOT fabricate numbers: every value comes from a
real run-manifest. Experiments that cannot run (e.g. a repo missing) are recorded as skipped with a reason.

  E0  discovery + filtering counts (from any run's summary)
  E1  syntax-only, sequential (workers=1) — correctness + peak throughput baseline
  E1p syntax-only, parallel (workers=N) — sequential-vs-bounded-parallel comparison
  E2  semantic enrichment of a fixed subset (semantic.subset_fraction)
  E3  full semantic enrichment
  E4  editor_snapshot vs strict_prefix leakage comparison (both policies are emitted in E2/E3)

Usage: python3 -I scripts/benchmark.py --repo data/repos/ttlcache [--repo ...] [--workers 8] [--subset 0.15]
"""
import argparse
import json
import os
import subprocess
import sys
import tempfile
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BIN = os.path.join(ROOT, "bin", "goflc")


def cfg_with(**over):
    base = json.loads(subprocess.check_output([BIN, "config"]))
    for k, v in over.items():
        cur = base
        parts = k.split(".")
        for p in parts[:-1]:
            cur = cur[p]
        cur[parts[-1]] = v
    f = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False)
    json.dump(base, f)
    f.close()
    return f.name


def extract(repo, out, cfg, workers, semantic):
    t = time.time()
    r = subprocess.run([BIN, "extract", "--repo", repo, "--out", out, "--config", cfg, "--repository-id",
                        "bench/" + os.path.basename(repo), "--overwrite", "--workers", str(workers),
                        "--semantic", semantic], capture_output=True, text=True)
    if r.returncode != 0:
        return None, r.stderr.strip()
    man = json.load(open(os.path.join(out, "run-manifest.json")))
    summ = json.load(open(os.path.join(out, "summary.json")))
    man["counters"] = summ["counters"]
    man["semantic"] = summ.get("semantic", {})
    man["_client_wall_s"] = round(time.time() - t, 3)
    return man, None


def validate(out, repo):
    r = subprocess.run([BIN, "validate", "--dataset", out, "--repo", repo], capture_output=True, text=True)
    return json.loads(r.stdout) if r.stdout else {"ok": False, "error": r.stderr.strip()}


def row(man, val=None):
    if man is None:
        return {"status": "skipped"}
    c = man["counters"] if "counters" in man else {}
    d = {
        "status": "ok",
        "wall_ms": man["timings"]["wall_ms"],
        "stages_cpu_ms": man["timings"]["stages"],
        "file_latency_ms": man["timings"]["file_latency_ms"],
        "samples_per_sec": round(man["throughput"]["samples_per_sec"], 1),
        "source_mib_per_sec": round(man["throughput"]["source_mib_per_sec"], 3),
        "peak_rss_mb": round(man["resources"]["peak_rss_bytes"] / 1e6, 1),
        "input_bytes": man["resources"]["input_bytes_discovered"],
        "workers": man["workers"],
    }
    if val is not None:
        d["validation_ok"] = val.get("ok")
        d["validation_failures"] = val.get("failures", {})
        d["semantic_records"] = val.get("semantic_records", 0)
    return d


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", action="append", required=True)
    ap.add_argument("--workers", type=int, default=max(2, os.cpu_count() // 2))
    ap.add_argument("--subset", type=float, default=0.15)
    ap.add_argument("--out", default=os.path.join(ROOT, "artifacts", "benchmarks"))
    args = ap.parse_args()
    os.makedirs(args.out, exist_ok=True)
    scratch = tempfile.mkdtemp(prefix="goflc-bench-")

    syntax = cfg_with()
    subset = cfg_with(**{"semantic.subset_fraction": args.subset})
    full = cfg_with()
    editor = cfg_with(**{"semantic.policies": ["editor_snapshot"]})  # bulk configuration

    results = {"generator": "scripts/benchmark.py", "bin": BIN, "repos": {}, "experiments": {}}
    for repo in args.repo:
        name = os.path.basename(repo)
        rr = {}
        # E1 sequential (cold then warm; report warm)
        o = os.path.join(scratch, name + "-syn")
        extract(repo, o, syntax, 1, "none")  # cold
        man, err = extract(repo, o, syntax, 1, "none")  # warm
        rr["E1_syntax_seq"] = row(man, validate(o, repo)) if man else {"status": "skipped", "reason": err}
        if man:
            results["repos"][name] = {
                "revision": man["revision"], "license": man["license"], "module_path": man["module_path"],
                "files_accepted": man["counters"].get("files.accepted"),
                "files_test": man["counters"].get("files.accepted_test"),
                "files_skipped": {k[len("files.skipped."):]: v for k, v in man["counters"].items()
                                  if k.startswith("files.skipped.")},
                "samples_total": man["counters"].get("samples.total"),
                "samples_by_kind": {k[len("samples.kind."):]: v for k, v in man["counters"].items()
                                    if k.startswith("samples.kind.")},
            }
        # E1 parallel
        o = os.path.join(scratch, name + "-synp")
        extract(repo, o, syntax, args.workers, "none")
        man, err = extract(repo, o, syntax, args.workers, "none")
        rr["E1_syntax_par"] = row(man, validate(o, repo)) if man else {"status": "skipped", "reason": err}
        # E2 semantic subset
        o = os.path.join(scratch, name + "-sem2")
        man, err = extract(repo, o, subset, args.workers, "best_effort")
        rr["E2_semantic_subset"] = row(man, validate(o, repo)) if man else {"status": "skipped", "reason": err}
        if man:
            rr["E2_semantic_subset"]["semantic_counters"] = {k: v for k, v in man["semantic"].items()}
        # E3e full semantic, editor_snapshot only (the bulk-run configuration), sequential and parallel
        for tag, w in (("E3e_semantic_editor_seq", 1), ("E3e_semantic_editor_par", args.workers)):
            o = os.path.join(scratch, name + "-" + tag)
            man, err = extract(repo, o, editor, w, "best_effort")
            rr[tag] = row(man, validate(o, repo)) if man else {"status": "skipped", "reason": err}
        # E3 full semantic
        o = os.path.join(scratch, name + "-sem3")
        man, err = extract(repo, o, full, args.workers, "best_effort")
        val = validate(o, repo) if man else None
        rr["E3_semantic_full"] = row(man, val) if man else {"status": "skipped", "reason": err}
        if man:
            sem = man["semantic"]
            rr["E3_semantic_full"]["semantic_counters"] = sem
            # E4 leakage / policy comparison (both policies ran in E3)
            rr["E4_leakage"] = {
                "editor_snapshot_resolved": sem.get("semantic.editor_snapshot.resolved", 0),
                "editor_snapshot_partial": sem.get("semantic.editor_snapshot.partially_resolved", 0),
                "strict_prefix_resolved": sem.get("semantic.strict_prefix.resolved", 0),
                "strict_prefix_partial": sem.get("semantic.strict_prefix.partially_resolved", 0),
                "editor_target_overlap": sem.get("semantic.editor_snapshot.target_identifier_overlap", 0),
                "strict_target_overlap": sem.get("semantic.strict_prefix.target_identifier_overlap", 0),
                "leakage_violations": sem.get("semantic.leakage_violations", 0),
                "validation_ok": val.get("ok") if val else None,
            }
        results["experiments"][name] = rr

    json.dump(results, open(os.path.join(args.out, "benchmark.json"), "w"), indent=2)
    write_md(results, os.path.join(args.out, "benchmark.md"))
    print("wrote", os.path.join(args.out, "benchmark.json"))


def write_md(res, path):
    L = ["# Go FLC pilot benchmark", "",
         "Every number is parsed from a real `run-manifest.json`; warm runs reported. See `benchmark.json`.", ""]
    for name, rr in res["experiments"].items():
        meta = res["repos"].get(name, {})
        L.append(f"## {name}")
        L.append(f"- revision `{meta.get('revision')}`, license {meta.get('license')}, "
                 f"{meta.get('files_accepted')} files ({meta.get('files_test')} test), "
                 f"{meta.get('samples_total')} samples")
        L.append("")
        L.append("| experiment | workers | wall ms | samples/s | MiB/s | peak RSS MB | validate | sem records |")
        L.append("| --- | ---: | ---: | ---: | ---: | ---: | :---: | ---: |")
        for ek in ("E1_syntax_seq", "E1_syntax_par", "E2_semantic_subset", "E3_semantic_full", "E3e_semantic_editor_seq",
                   "E3e_semantic_editor_par"):
            r = rr.get(ek, {})
            if r.get("status") != "ok":
                L.append(f"| {ek} | | | | | | skipped | |")
                continue
            L.append(f"| {ek} | {r['workers']} | {r['wall_ms']:.0f} | {r['samples_per_sec']:.0f} | "
                     f"{r['source_mib_per_sec']:.3f} | {r['peak_rss_mb']:.0f} | {r.get('validation_ok')} | "
                     f"{r.get('semantic_records','')} |")
        e4 = rr.get("E4_leakage")
        if e4:
            L.append("")
            L.append(f"**E4 leakage/policy:** editor_snapshot resolved {e4['editor_snapshot_resolved']} / "
                     f"partial {e4['editor_snapshot_partial']}; strict_prefix resolved {e4['strict_prefix_resolved']} "
                     f"/ partial {e4['strict_prefix_partial']}; target-identifier overlap "
                     f"editor={e4['editor_target_overlap']} strict={e4['strict_target_overlap']}; "
                     f"**leakage violations {e4['leakage_violations']}** (validate ok={e4['validation_ok']}).")
        L.append("")
    open(path, "w").write("\n".join(L))


if __name__ == "__main__":
    sys.exit(main())
