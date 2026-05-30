#!/usr/bin/env python3
"""Fetch HuggingFace datasets and convert them to the SQuAD-style JSON that
msc-bench / msc-qa consume (data[].paragraphs[].{context, qas[].{question,
answers}}), so a diverse set of recall/injection regimes can be seeded into
MuninnDB vaults and measured.

Usage:
    python3 scripts/fetch_hf_datasets.py <name> [out.json] [--pages N]
    python3 scripts/fetch_hf_datasets.py all            # write /tmp/<name>.json for every dataset

Then seed + probe, e.g.:
    go run ./cmd/msc-bench -seed -probe -corpus squad \\
        -squad-file /tmp/sciq.json -vault msc-sciq -squad-articles 200 \\
        -n 300 -present 80 -absent 80 -mode semantic

Each converter maps a dataset to (context, question, answer): the context is
seeded as a memory, the question is the recall probe, the answer is the gold
span. Datasets span distinct regimes — extractive QA, science QA, fact
verification, scientific retrieval, instruction-following — so retrieval
difficulty and the per-vault gate vary (see docs/model-eval.md, docs/experiments.md).
"""
import json
import sys
import urllib.request


def _fetch(ds, cfg, split, pages):
    """Page through the HF datasets-server rows API (100 rows/call)."""
    rows = []
    for off in range(0, pages * 100, 100):
        url = (f"https://datasets-server.huggingface.co/rows?dataset={ds}"
               f"&config={cfg}&split={split}&offset={off}&length=100")
        try:
            with urllib.request.urlopen(url, timeout=30) as r:
                batch = json.load(r).get("rows", [])
        except Exception as e:  # noqa: BLE001 — best-effort fetch
            sys.stderr.write(f"warn: {ds} offset {off}: {str(e)[:80]}\n")
            break
        if not batch:
            break
        rows += [b["row"] for b in batch]
    return rows


def _article(i, prefix, context, question, answer):
    return {"title": f"{prefix}-{i}",
            "paragraphs": [{"context": context,
                            "qas": [{"question": question, "is_impossible": False,
                                     "answers": [{"text": answer}]}]}]}


def conv_sciq(pages):
    """allenai/sciq — science exam questions with a `support` passage."""
    out = []
    for i, r in enumerate(_fetch("allenai/sciq", "default", "validation", pages)):
        s = (r.get("support") or "").strip()
        if len(s) < 20:
            continue
        out.append(_article(i, "sciq", s, r["question"], r["correct_answer"]))
    return out


def conv_fever(pages):
    """copenlu/fever_gold_evidence — claim verification; evidence is the memory."""
    out = []
    for i, r in enumerate(_fetch("copenlu/fever_gold_evidence", "default", "train", pages)):
        if r.get("label") not in ("SUPPORTS", "REFUTES"):
            continue
        ev = r.get("evidence") or []
        txt = " ".join(e[2] for e in ev if isinstance(e, list) and len(e) >= 3 and e[2])
        if len(txt) < 20:
            continue
        out.append(_article(i, "fever", txt, r["claim"], r["label"]))
    return out


def conv_scifact(pages):
    """BeIR/scifact-generated-queries — scientific abstract retrieval."""
    out = []
    for i, r in enumerate(_fetch("BeIR/scifact-generated-queries", "default", "train", pages)):
        text = (r.get("text") or "").strip()
        q = (r.get("query") or "").strip()
        if len(text) < 20 or not q:
            continue
        # Retrieval-only: gold "answer" is the title (a coarse answer-presence check).
        out.append(_article(i, "scifact", text, q, (r.get("title") or "")[:60]))
    return out


def conv_dolly(pages):
    """databricks-dolly-15k closed_qa — instruction + context + response."""
    out = []
    for i, r in enumerate(_fetch("databricks/databricks-dolly-15k", "default", "train", pages)):
        if r.get("category") != "closed_qa":
            continue
        ctx = (r.get("context") or "").strip()
        instr = (r.get("instruction") or "").strip()
        resp = (r.get("response") or "").strip()
        if len(ctx) < 20 or not instr or not resp:
            continue
        out.append(_article(i, "dolly", ctx, instr, resp))
    return out


def conv_wikiqa(pages):
    """microsoft/wiki_qa — keep answer sentences labeled relevant (label==1)."""
    out = []
    for i, r in enumerate(_fetch("microsoft/wiki_qa", "default", "validation", pages)):
        if int(r.get("label", 0)) != 1:
            continue
        ans = (r.get("answer") or "").strip()
        q = (r.get("question") or "").strip()
        if len(ans) < 20 or not q:
            continue
        out.append(_article(i, "wikiqa", ans, q, ans[:60]))
    return out


def conv_narrativeqa(pages):
    """deepmind/narrativeqa — long-narrative QA in the *summary* setting (the
    full text is ~75k words; the human summary is the seedable context). One
    article per document (summaries repeat across a book's questions)."""
    out = []
    seen = set()
    for r in _fetch("deepmind/narrativeqa", "default", "validation", pages):
        doc = r.get("document") or {}
        summ = doc.get("summary") or {}
        text = (summ.get("text") or "").strip()
        did = doc.get("id")
        if len(text) < 40 or did in seen:
            continue
        seen.add(did)
        q = r.get("question") or {}
        qt = (q.get("text") or "").strip() if isinstance(q, dict) else str(q)
        ans = r.get("answers") or []
        at = (ans[0].get("text") or "").strip() if ans and isinstance(ans[0], dict) else ""
        if not qt or not at:
            continue
        out.append(_article(len(out), "narrativeqa", text, qt, at))
    return out


def conv_medical(pages):
    """lavita/medical-qa-datasets — medical-domain QA; keep doctor-answer items
    (input = patient question, output = answer)."""
    out = []
    for r in _fetch("lavita/medical-qa-datasets", "all-processed", "train", pages):
        instr = (r.get("instruction") or "").lower()
        if "answer the medical question" not in instr:
            continue
        q = (r.get("input") or "").strip()
        a = (r.get("output") or "").strip()
        if len(a) < 30 or len(q) < 10:
            continue
        out.append(_article(len(out), "medical", a, q, a[:60]))
    return out


CONVERTERS = {
    "sciq": conv_sciq,
    "fever": conv_fever,
    "scifact": conv_scifact,
    "dolly": conv_dolly,
    "wikiqa": conv_wikiqa,
    "narrativeqa": conv_narrativeqa,
    "medical": conv_medical,
}


def main(argv):
    if len(argv) < 2 or argv[1] not in (*CONVERTERS, "all"):
        sys.stderr.write(f"usage: {argv[0]} <{'|'.join(CONVERTERS)}|all> [out.json] [--pages N]\n")
        return 2
    pages = 5
    if "--pages" in argv:
        pages = int(argv[argv.index("--pages") + 1])
    names = list(CONVERTERS) if argv[1] == "all" else [argv[1]]
    for name in names:
        data = CONVERTERS[name](pages)
        out = argv[2] if len(argv) > 2 and not argv[2].startswith("--") and argv[1] != "all" else f"/tmp/{name}.json"
        with open(out, "w") as f:
            json.dump({"data": data}, f)
        print(f"{name}: {len(data)} examples -> {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
