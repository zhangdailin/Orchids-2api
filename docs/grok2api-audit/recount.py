#!/usr/bin/env python3
"""Recompute the audit/fix tally for the Grok channel parity audit.

Reads:
  docs/grok2api-parity-audit.md   -> every finding heading `### A?-? [P?]`
  docs/grok2api-fix-ledger.md     -> "fixed" tables (sections 一 and 五..十一 plus
                                     section 三's "已解决的旧条目"), "kept" tables
                                     (section 二 and section 三's "有意保留"),
                                     "remaining" table (section 三 before
                                     "### 已解决的旧条目")

Prints the disjoint classification and asserts the three sets partition the audit.
Run from the repository root:  python3 docs/grok2api-audit/recount.py
"""

import re
import sys
from collections import Counter
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
AUDIT = ROOT / "docs" / "grok2api-parity-audit.md"
LEDGER = ROOT / "docs" / "grok2api-fix-ledger.md"

ID = r"[A-Z]\d+-\d+"


def section(lines, heading, start=0):
    for idx in range(start, len(lines)):
        if lines[idx].startswith(heading):
            return idx
    raise SystemExit(f"heading not found: {heading}")


def table_ids(lines, begin, end):
    """Finding ids appearing in the first cell of a markdown table row."""
    found = set()
    for line in lines[begin:end]:
        if not line.startswith("|"):
            continue
        cells = line.split("|")
        if len(cells) > 1:
            found |= set(re.findall(ID, cells[1]))
    return found


def main():
    severity = dict(
        re.findall(rf"^### ({ID})\s+\[([^\]]+)\]", AUDIT.read_text(encoding="utf-8"), re.M)
    )
    lines = LEDGER.read_text(encoding="utf-8").splitlines()

    s1 = section(lines, "## 一")
    s2 = section(lines, "## 二")
    s3 = section(lines, "## 三")
    s4 = section(lines, "## 四")
    s5 = section(lines, "## 五")
    resolved = section(lines, "### 已解决的旧条目", s3)
    deliberate = section(lines, "### 有意保留", s3)

    fixed = (
        table_ids(lines, s1, s2)          # round 1
        | table_ids(lines, s5, len(lines))  # rounds 2..8
        | table_ids(lines, resolved, deliberate)
        | {"A4-10"}                        # closed by a prose note in section 九
    )
    kept = table_ids(lines, s2, s3) | table_ids(lines, deliberate, s4)
    remaining = table_ids(lines, s3, resolved)

    total = set(severity)
    fixed_d = fixed & total - kept - remaining
    kept_d = kept & total - remaining
    remaining_d = remaining & total

    def report(label, ids):
        dist = Counter(severity[i].split("/")[0] for i in ids)
        order = ("P0", "P1", "P2", "P3")
        cells = "、".join(f"{p} ×{dist[p]}" for p in order if dist[p])
        print(f"{label:<10} {len(ids):>4}  {cells}")

    report("总计", total)
    report("已修复", fixed_d)
    report("有意保留", kept_d)
    report("未修", remaining_d)

    print("\n未修清单：", "、".join(sorted(remaining_d)))
    print("有意保留：", "、".join(sorted(kept_d)))

    unaccounted = total - fixed_d - kept_d - remaining_d
    if unaccounted:
        print("\n未归类：", "、".join(sorted(unaccounted)))
        return 1
    bad = [i for i in fixed_d if not severity[i]]
    if bad:
        print("\n有修复记录但缺审计标题：", "、".join(sorted(bad)))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
