"""Assert facts about a PathGauge results file.

Usage: python3 -I check.py RESULTS.json 'EXPR' ['EXPR' ...]

Each EXPR is a Python expression evaluated with these helpers:
  res(test, direction=None, streams=None, index=0)  result dict of an ok step
  st(test, ...)                                     the step dict itself
  finding(prefix)                                   True if a finding ID starts with prefix
  sev(prefix)                                       severity of the first such finding
  approx(x, target, pct)                            |x-target| <= pct% of target
  grade_rank(g)                                     0 for A+ ... 5 for F
  S                                                 the run summary dict
  R                                                 the whole report
  steps_ok()                                        True if every step succeeded
Exit status is 1 if any expression is false or fails.
"""
import json
import sys


def main():
    path, exprs = sys.argv[1], sys.argv[2:]
    with open(path) as fh:
        report = json.load(fh)

    def matching(test, direction=None, streams=None):
        out = []
        for s in report.get("steps", []):
            if s.get("test") != test or s.get("status") != "ok":
                continue
            r = s.get("result") or {}
            if direction and r.get("direction") != direction:
                continue
            if streams is not None and r.get("streams") != streams:
                continue
            out.append(s)
        return out

    def st(test, direction=None, streams=None, index=0):
        m = matching(test, direction, streams)
        if len(m) <= index:
            raise LookupError(f"no ok step for {test} {direction or ''} {streams or ''}")
        return m[index]

    def res(test, direction=None, streams=None, index=0):
        return st(test, direction, streams, index)["result"]

    def finding(prefix):
        return any(f["id"].startswith(prefix) for f in report.get("findings", []))

    def sev(prefix):
        for f in report.get("findings", []):
            if f["id"].startswith(prefix):
                return f["severity"]
        return None

    def approx(x, target, pct):
        return abs(x - target) <= abs(target) * pct / 100

    def grade_rank(g):
        order = ["A+", "A", "B", "C", "D", "F"]
        return order.index(g) if g in order else 99

    def steps_ok():
        bad = [f"{s['id']}: {s.get('error')}" for s in report.get("steps", []) if s.get("status") != "ok"]
        if bad:
            print("      failed steps: " + "; ".join(bad))
        return not bad

    env = {"res": res, "st": st, "finding": finding, "sev": sev, "approx": approx, "grade_rank": grade_rank,
           "S": report.get("summary", {}), "R": report, "steps_ok": steps_ok, "len": len, "abs": abs,
           "all": all, "any": any, "min": min, "max": max, "set": set}
    failed = 0
    for e in exprs:
        try:
            ok = bool(eval(e, {"__builtins__": {}}, env))
            detail = ""
        except Exception as exc:  # noqa: BLE001 - report any evaluation error
            ok, detail = False, f" ({type(exc).__name__}: {exc})"
        print(f"    {'PASS' if ok else 'FAIL'}  {e}{detail}")
        failed += not ok
    ids = [f"{f['severity']}:{f['id']}" for f in report.get("findings", [])]
    print(f"    findings: {', '.join(ids)}")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
