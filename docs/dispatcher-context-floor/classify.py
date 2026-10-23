#!/usr/bin/env python3
"""Check classes.tsv against DISPATCHER_PROTOCOL.md and write measurement chunks.

Usage: classify.py <outdir>

Verifies every protocol line is in exactly one unit, prints bytes per unit,
class and section, and writes one chunk file per class (class-a.md, ...) and
per top-level section (section-<n>.md) into <outdir>, for
`CHUNK=<file> measure.sh chunk` real-token runs.
"""

import csv
import pathlib
import sys

here = pathlib.Path(__file__).resolve().parent
proto = here.parents[1] / "adapters/core/protocols/DISPATCHER_PROTOCOL.md"
lines = proto.read_text(encoding="utf-8").split("\n")
if lines and lines[-1] == "":
    lines.pop()

rows = list(csv.DictReader((here / "classes.tsv").open(encoding="utf-8"), delimiter="\t"))
seen = [0] * len(lines)
for r in rows:
    for i in range(int(r["start"]) - 1, int(r["end"])):
        seen[i] += 1
bad = [i + 1 for i, n in enumerate(seen) if n != 1]
if bad:
    sys.exit(f"classify.py: lines not covered exactly once: {bad[:20]}")

out = pathlib.Path(sys.argv[1])
out.mkdir(parents=True, exist_ok=True)


def text(r):
    return "\n".join(lines[int(r["start"]) - 1 : int(r["end"])]) + "\n"


by_class, by_section, order = {}, {}, []
for r in rows:
    t = text(r)
    by_class.setdefault(r["class"], []).append(t)
    if r["section"] not in by_section:
        order.append(r["section"])
    by_section.setdefault(r["section"], []).append(t)
    print(f"{r['start']}-{r['end']}\t{r['class']}\t{len(t.encode())}\t{r['unit']}")

for c, ts in sorted(by_class.items()):
    (out / f"class-{c}.md").write_text("".join(ts), encoding="utf-8")
    print(f"class {c}\t{sum(len(t.encode()) for t in ts)} bytes")
for n, s in enumerate(order):
    ts = by_section[s]
    (out / f"section-{n}.md").write_text("".join(ts), encoding="utf-8")
    print(f"section-{n}\t{s}\t{sum(len(t.encode()) for t in ts)} bytes")
