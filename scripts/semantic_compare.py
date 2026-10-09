#!/usr/bin/env python3
"""Compare semantic.jsonl of two runs (e.g. --semantic-engine auto vs snapshot) per (sample_id, policy).

Reports the share of records with identical facts and, for differing records, which fields differ and whether the
candidate run is a subset ("more conservative") or a superset of the reference. Usage:
  python3 -I scripts/semantic_compare.py REF_DIR CAND_DIR
"""
import collections
import json
import sys

FIELDS = ["status", "enclosing_symbol", "return_type", "expected_type", "receiver_type", "receiver_kind",
          "call_signature", "argument_index", "locals", "parameters", "receiver_members", "package_members", "members",
          "context_types"]


def load(d):
    out = {}
    for line in open(d + "/semantic.jsonl", encoding="utf-8"):
        r = json.loads(line)
        out[(r["sample_id"], r["visibility_policy"])] = r
    return out


def norm(v):
    if isinstance(v, list):
        return sorted(json.dumps(x, sort_keys=True) for x in v)
    return v


def main():
    ref, cand = load(sys.argv[1]), load(sys.argv[2])
    same = 0
    diff_fields = collections.Counter()
    rel = collections.Counter()
    engines = collections.Counter(r.get("analysis_engine") for r in cand.values())
    examples = []
    for k, r in ref.items():
        c = cand.get(k)
        if c is None:
            diff_fields["missing"] += 1
            continue
        d = [f for f in FIELDS if norm(r.get(f)) != norm(c.get(f))]
        if not d:
            same += 1
            continue
        for f in d:
            diff_fields[f] += 1
            a, b = norm(r.get(f)), norm(c.get(f))
            if isinstance(a, list):
                sa, sb = set(a), set(b)
                rel[(f, "cand_subset" if sb < sa else "cand_superset" if sb > sa else "other")] += 1
        if len(examples) < 8:
            examples.append({"key": k, "fields": d, "ref_status": r.get("status"), "cand_status": c.get("status"),
                             "cand_engine": c.get("analysis_engine")})
    n = len(ref)
    print(json.dumps({"records": n, "identical": same, "identical_pct": round(100 * same / max(1, n), 2),
                      "differing_fields": dict(diff_fields), "list_relation": {f"{a}:{b}": v for (a, b), v in rel.items()},
                      "candidate_engines": dict(engines), "examples": examples}, indent=1))


if __name__ == "__main__":
    main()
