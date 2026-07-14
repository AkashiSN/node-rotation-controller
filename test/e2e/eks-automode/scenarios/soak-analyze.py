#!/usr/bin/env python3
"""Scenario P (issue #118) offline analyzer. Usage: soak-analyze.py <run-dir>.

Inputs (run-dir):
  controller.log   kubectl logs --prefix --timestamps stream (soak-record.sh);
                    "== <ts> (re)start:" header lines interleaved with zap JSON.
  snapshots.jsonl   one JSON object per line, {ts, kind, items} with kind in
                    {nodeclaims, nodes, pods, rotationpolicies}; error lines are
                    {ts, kind, error: true} (soak-record.sh, every 120s).
  scrape.log        in-cluster scraper output (soak-scraper.yaml), harvested via
                    `kubectl logs` and placed here by the runbook: lines are
                    "<seq> <ts> <pod> <metric-or-SCRAPE_ERROR...>" where seq is a
                    monotone per-scrape-round counter and metric lines are raw
                    Prometheus text ("name{labels} value" or "name value").

Output: <run-dir>/report.md — per-rotation ledger, margin distribution, and a
PASS/FAIL table for the spec rev4 §6 criteria evaluable offline (1, 2, 3, 4, 5,
7a, 8, 9; the rest need live-cluster/Event evidence this analyzer does not have).

Counters (noderotation_completed_total, noderotation_forceful_fallback_total)
are summed as PER-POD deltas, reset-tolerant: a reading below the previous one
starts a new epoch and contributes its own value rather than a negative delta
(spec §4: "each process owns independent counters and leadership can in
principle move without a restart"). Absent series are a 0 baseline.

Gauges (the criterion-4 and 7a series) are the opposite: they are read as
instantaneous per-scrape values, never delta-summed. Only the leading replica
(replicaCount=2, leader election) actively Sets most rotation-state gauges; a
standby's registry reports the Go zero-value or omits the series, so this
script takes the MAX across pods at each scrape round to recover the true
value regardless of which replica currently leads.
"""
import json
import re
import statistics
import sys
from datetime import datetime

# Deadline offset for the margin criterion: old claim's creationTimestamp + 2h12m
# (spec derivation for issue #118; E = 7920s).
E_SECONDS = 7920

# Prometheus "nodepool" label value of the pool under soak. Exact-match only:
# "nodepool-soak-epi" (the epilogue mini-pool) shares this as a string prefix.
MAIN_POOL = "nodepool-soak"

# Karpenter's own label key on NodeClaim/Node objects (karpenter.sh/nodepool) —
# distinct from the Prometheus metric label "nodepool" used above; snapshots.jsonl
# carries live Kubernetes objects, so census code must key off this one instead.
K8S_POOL_LABEL = "karpenter.sh/nodepool"

# This controller's own annotation recording per-NodeClaim rotation progress
# (internal/annotations/annotations.go: State = "noderotation.io/state", values
# StatePending="pending", StateDraining="draining", StateFailed="failed",
# StateExpired="expired"; empty/absent = fresh, no rotation ever anchored here).
STATE_ANNOTATION = "noderotation.io/state"
STATE_IN_FLIGHT = {"pending", "draining"}

# Exact structured-log keys of the "rotation complete" line (Task 7 Step 1,
# pinned from source, not guessed):
#   internal/controller/rotation_controller.go:1131
#     kv := []any{"nodeclaim", name, "mode", rotationMode(pool)}
#   ...:1133  kv = append(kv, "surgeNode", surgeNode)              (omitted on
#             the surge-less forceful-fallback path, which has no surge phase)
#   ...:1148  kv = append(kv, "total", (surgeWait+drain).Round(...).String())
#             (omitted unless both surgeWait and drain are known)
#   internal/controller/rotation_controller.go:1168
#     log.FromContext(ctx).WithValues("nodepool", pool.Name).Info("rotation complete", kv...)
# Confirmed against internal/controller/transitionlog_internal_test.go:333
# (TestRotationCompleteIsLogged), which asserts exactly these key names.
LEDGER_KEYS = {"claim": "nodeclaim", "pool": "nodepool", "surge": "surgeNode", "total": "total"}

# The six spec rev4 §6.7a gauges and their pinned canonical values for this
# soak's derivation (A/P/t_rot_bound/t_rot_estimate/C/G — see docs/superpowers/
# specs/2026-07-14-118-tight-race-soak-design.md §1). Metric names and the shared
# "nodepool" label are pinned from internal/metrics/metrics.go (New/ObservePool).
GAUGE_TARGETS = {
    "noderotation_age_threshold_seconds": 3600.0,
    "noderotation_window_period_seconds": 1800.0,
    "noderotation_t_rot_bound_seconds": 720.0,
    "noderotation_t_rot_estimate_seconds": 600.0,
    "noderotation_throughput_capacity": 3.0,
    "noderotation_rotation_chances": 2.0,
}

# The two counter-family series this analyzer delta-sums; every other
# noderotation_* series (gauges, histogram _bucket/_sum/_count) is instantaneous
# and must NOT be treated as a monotone counter.
COUNTER_METRICS = {"noderotation_completed_total", "noderotation_forceful_fallback_total"}

_METRIC_LINE_RE = re.compile(r"^([A-Za-z_:][A-Za-z0-9_:]*)(?:\{(.*)\})?\s+(\S+)$")
_LABEL_RE = re.compile(r'([A-Za-z_][A-Za-z0-9_]*)="((?:[^"\\]|\\.)*)"')


def parse_ts(s):
    """Parse a zap 'ts' field. This repo's logger (cmd/main.go: ctrl.SetLogger(
    zap.New(zap.UseFlagOptions(&zapOpts)))) never sets Options.TimeEncoder, and
    controller-runtime's zap.Options.addDefaults falls back to
    zapcore.RFC3339TimeEncoder unconditionally — the chart's --zap-devel flag
    (charts/.../deployment.yaml, values.yaml logging.development, default false)
    only changes level/encoder(console vs JSON)/stacktrace defaults, not the time
    encoder. So "ts" is always an RFC3339 string here, never an epoch number."""
    if not isinstance(s, str):
        raise TypeError(
            f"unexpected non-string zap 'ts' field: {s!r} — this repo's zap "
            "setup always emits RFC3339TimeEncoder strings (see cmd/main.go); "
            "a non-string ts means the input does not match that assumption"
        )
    return datetime.fromisoformat(s.replace("Z", "+00:00"))


def parse_metric_line(text):
    """Parse a raw Prometheus exposition line: 'name{k="v",...} value' or
    'name value'. Returns (name, labels-dict, float-value) or None if it doesn't
    match (e.g. truncated line, unexpected format)."""
    m = _METRIC_LINE_RE.match(text.strip())
    if not m:
        return None
    name, label_str, val_str = m.groups()
    labels = dict(_LABEL_RE.findall(label_str)) if label_str else {}
    try:
        val = float(val_str)
    except ValueError:
        return None
    return name, labels, val


def load_claim_births(run):
    """{claim-name: creationTimestamp} from every nodeclaims snapshot. Deleted
    claims cannot be queried later, so births must come from whichever snapshot
    caught them before deletion (soak-record.sh runs every 120s); setdefault
    keeps the earliest sighting, though creationTimestamp is immutable so any
    sighting would agree."""
    births = {}
    for line in open(f"{run}/snapshots.jsonl"):
        line = line.strip()
        if not line:
            continue
        try:
            snap = json.loads(line)
        except json.JSONDecodeError:
            continue
        if snap.get("kind") != "nodeclaims" or snap.get("error") or "items" not in snap:
            continue
        for it in snap["items"]:
            meta = it.get("metadata", {})
            name, created = meta.get("name"), meta.get("creationTimestamp")
            if name and created:
                births.setdefault(name, parse_ts(created))
    return births


def load_last_nodeclaims_snapshot(run):
    """The LAST valid (non-error) nodeclaims snapshot — the criterion-9 end
    census. Returns None if snapshots.jsonl has no such line."""
    last = None
    for line in open(f"{run}/snapshots.jsonl"):
        line = line.strip()
        if not line:
            continue
        try:
            snap = json.loads(line)
        except json.JSONDecodeError:
            continue
        if snap.get("kind") == "nodeclaims" and not snap.get("error") and "items" in snap:
            last = snap
    return last


def load_ledger(run):
    """Parse zap JSON lines out of the --prefix'ed controller.log; one row per
    "rotation complete" line. raw.find("{") skips both the kubectl --prefix tag
    (e.g. "[pod/x] 2026-...Z ") and the recorder's own "== <ts> (re)start: ..."
    headers, neither of which contains a brace before the JSON payload starts.

    Returns (rows, bad-ts-count, mentions): mentions counts every raw line
    containing the text "rotation complete" regardless of parseability, so the
    caller can fail LOUD when the file clearly holds rotations the parser could
    not attribute — the signature of zap CONSOLE format (--zap-devel /
    logging.development: true), where the line is "<ts>\\tINFO\\trotation
    complete\\t{...kv...}": the trailing {...} parses as JSON but carries no
    "msg"/"ts" key, so a silent-empty ledger would false-FAIL criterion 8."""
    rows, bad, mentions = [], 0, 0
    for raw in open(f"{run}/controller.log"):
        if "rotation complete" in raw:
            mentions += 1
        i = raw.find("{")
        if i < 0:
            continue
        try:
            j = json.loads(raw[i:])
        except json.JSONDecodeError:
            continue
        if j.get("msg") != "rotation complete":
            continue
        try:
            ts = parse_ts(j["ts"])
        except (KeyError, TypeError, ValueError):
            bad += 1
            continue
        rows.append(
            {
                "ts": ts,
                "claim": j.get(LEDGER_KEYS["claim"]),
                "pool": j.get(LEDGER_KEYS["pool"], ""),
                "mode": j.get("mode", ""),
                "surge": j.get(LEDGER_KEYS["surge"], ""),
                "total": j.get(LEDGER_KEYS["total"], ""),
            }
        )
    return rows, bad, mentions


def scan_scrape_log(run):
    """One pass over scrape.log, in CHRONOLOGICAL order. Returns:
      events    list of {epoch,seq,ts,pod,name,labels,value} for metric lines
      seq_ts    {(epoch, seq): ts} for every seq seen (metric or SCRAPE_ERROR
                line) — used to turn a seq gap into a wall-clock duration
      restarts  list of {ts, prev_seq, new_seq} — one per detected scraper
                restart (the seq counter went BACKWARDS at a later timestamp;
                the scraper's seq is a shell variable that restarts at 1)
      error_count  total SCRAPE_ERROR lines (both "no-controller-pod" and
                "proxy" failure variants; soak-scraper.yaml)

    The runbook harvests `kubectl logs` and then APPENDS `--previous` after it
    (SCENARIOS.md Harvest step), so the OLDER scraper epoch lands AFTER the
    newer one in file order. Processing in file order would misread the drop
    back to the old epoch's lower counter values as a controller counter reset
    (double-counting every completed rotation) and the duplicated seq numbers
    would mask the restart gap. So all lines are stable-sorted by their ts
    field first — RFC3339 UTC timestamps sort lexicographically, and the
    stable sort keeps same-ts lines in their original relative order."""
    entries = []
    for raw in open(f"{run}/scrape.log"):
        line = raw.rstrip("\n")
        if not line:
            continue
        parts = line.split(maxsplit=3)
        if len(parts) < 4:
            continue  # malformed line: skip rather than abort a 12h analysis
        seq_s, ts, pod, rest = parts
        try:
            seq = int(seq_s)
        except ValueError:
            continue
        entries.append((ts, seq, pod, rest.strip()))
    entries.sort(key=lambda e: e[0])  # chronological; sort() is stable

    events, seq_ts, restarts, error_count = [], {}, [], 0
    epoch, last_seq = 0, None
    for ts, seq, pod, rest in entries:
        if last_seq is not None and seq < last_seq:
            epoch += 1
            restarts.append({"ts": ts, "prev_seq": last_seq, "new_seq": seq})
        last_seq = seq
        seq_ts.setdefault((epoch, seq), ts)
        if rest.startswith("SCRAPE_ERROR"):
            error_count += 1
            continue
        parsed = parse_metric_line(rest)
        if parsed is None:
            continue
        name, labels, val = parsed
        events.append({"epoch": epoch, "seq": seq, "ts": ts, "pod": pod,
                       "name": name, "labels": labels, "value": val})
    return events, seq_ts, restarts, error_count


def counter_totals(events):
    """{(pod, metric, sorted-label-tuple): total} — per-(pod,series) delta sums
    over the counter-family metrics only, reset-tolerant (v < prev ⇒ new epoch,
    contributes v rather than a negative delta). Requires events in
    CHRONOLOGICAL order (scan_scrape_log guarantees it): out-of-order input
    would make a mere step back in time look like a counter reset and
    double-count."""
    last, totals = {}, {}
    for e in events:
        if e["name"] not in COUNTER_METRICS:
            continue
        key = (e["pod"], e["name"], tuple(sorted(e["labels"].items())))
        v = e["value"]
        prev = last.get(key, 0.0)
        totals[key] = totals.get(key, 0.0) + (v - prev if v >= prev else v)
        last[key] = v
    return totals


def sum_counter(totals, name, label_filters=None):
    """Sum a counter's per-pod-series totals across all pods, filtered to series
    whose labels match label_filters exactly (not substring) on every given key.
    Absent series contribute 0 — the caller never needs a special case."""
    label_filters = label_filters or {}
    out = 0.0
    for (_, n, labels), v in totals.items():
        if n != name:
            continue
        ld = dict(labels)
        if all(ld.get(k) == want for k, want in label_filters.items()):
            out += v
    return out


def gauge_per_scrape(events, name, label_filters=None):
    """{(epoch, seq): max-value-across-pods} for one gauge series filtered by
    exact label match. Keyed by (epoch, seq), not bare seq: after a scraper
    restart the seq counter starts over at 1, so bare-seq keys would collapse
    readings from different scrape rounds into one. See the module docstring
    for why gauges use max-per-scrape, not deltas."""
    label_filters = label_filters or {}
    out = {}
    for e in events:
        if e["name"] != name:
            continue
        if any(e["labels"].get(k) != want for k, want in label_filters.items()):
            continue
        key = (e["epoch"], e["seq"])
        out[key] = max(out.get(key, e["value"]), e["value"])
    return out


def seq_gap_report(seq_ts):
    """Contiguous missing-seq ranges PER SCRAPER EPOCH, each annotated with the
    wall-clock duration spanned (bounding-ts after − bounding-ts before), so a
    gap can be judged against spec rev4 §6 criterion 5's "no recorder gap >
    5m". A restart boundary is NOT a missing-seq gap (the seq legitimately
    starts over); restart coverage gaps are computed by restart_gaps()."""
    out = []
    for ep in sorted({e for e, _ in seq_ts}):
        eseq = {s: t for (e, s), t in seq_ts.items() if e == ep}
        seqs = sorted(eseq)
        lo, hi = seqs[0], seqs[-1]
        present = set(seqs)
        missing = [n for n in range(lo, hi + 1) if n not in present]
        ranges = []
        for n in missing:
            if ranges and n == ranges[-1][1] + 1:
                ranges[-1] = (ranges[-1][0], n)
            else:
                ranges.append((n, n))
        for a, b in ranges:
            before, after = eseq.get(a - 1), eseq.get(b + 1)
            dur = (parse_ts(after) - parse_ts(before)).total_seconds() if before and after else None
            out.append({"epoch": ep, "from": a, "to": b,
                        "before_ts": before, "after_ts": after, "duration_s": dur})
    return out


def restart_gaps(seq_ts, restarts):
    """Wall-clock coverage gap around each scraper restart: last ts of the
    epoch before it → first ts of the epoch after it. With `--previous`
    harvested this equals the scraper's actual downtime; it counts against
    criterion 5's "no recorder gap > 5m" exactly like a missing-seq gap."""
    out = []
    for i, r in enumerate(restarts):
        prev_ep, new_ep = i, i + 1  # restarts[i] opened epoch i+1
        prev_ts = max((t for (e, _), t in seq_ts.items() if e == prev_ep), default=None)
        new_ts = min((t for (e, _), t in seq_ts.items() if e == new_ep), default=None)
        dur = (parse_ts(new_ts) - parse_ts(prev_ts)).total_seconds() if prev_ts and new_ts else None
        out.append({**r, "before_ts": prev_ts, "after_ts": new_ts, "duration_s": dur})
    return out


def build_ledger_rows(ledger, births):
    """Join each "rotation complete" row to its claim's birth and compute the
    margin = (birth + E) - completion. Rows whose claim never appeared in any
    nodeclaims snapshot are reported separately (missing) rather than silently
    dropped."""
    rows, missing = [], []
    for r in ledger:
        b = births.get(r["claim"])
        if b is None:
            missing.append(r["claim"])
            continue
        margin_s = (b.timestamp() + E_SECONDS) - r["ts"].timestamp()
        rows.append({**r, "birth": b, "margin_s": margin_s})
    return rows, missing


def census(run, last_snapshot):
    """Criterion 9: from the LAST nodeclaims snapshot, flag claims older than
    E_SECONDS... no — older than the ageThreshold A (3600s, the pinned canonical
    value from GAUGE_TARGETS) with no in-flight rotation (State not in
    {pending, draining}); younger claims are right-censored (still OK to be
    un-rotated, not yet due). Separately flag any claim in State=failed
    (noderotation.io/state) and, informationally, any in State=expired (directly
    evidences a criterion-1 violation)."""
    A = GAUGE_TARGETS["noderotation_age_threshold_seconds"]
    result = {"census_ts": None, "old_no_inflight": [], "failed": [], "expired": []}
    if last_snapshot is None:
        return result
    census_ts = parse_ts(last_snapshot["ts"])
    result["census_ts"] = census_ts
    for it in last_snapshot.get("items", []):
        meta = it.get("metadata", {})
        name, created = meta.get("name"), meta.get("creationTimestamp")
        if not name or not created:
            continue
        pool = (meta.get("labels") or {}).get(K8S_POOL_LABEL, "")
        state = (meta.get("annotations") or {}).get(STATE_ANNOTATION, "")
        age_s = (census_ts - parse_ts(created)).total_seconds()
        if state == "failed":
            result["failed"].append((name, pool, age_s))
        if state == "expired":
            result["expired"].append((name, pool, age_s))
        if age_s > A and state not in STATE_IN_FLIGHT:
            result["old_no_inflight"].append((name, pool, age_s, state or "(none)"))
    return result


def fmt_min(seconds):
    return seconds / 60.0


def main(run):
    births = load_claim_births(run)
    ledger, ledger_bad, ledger_mentions = load_ledger(run)
    ledger_warning = None
    if ledger_mentions > 0 and not ledger:
        ledger_warning = (
            f"WARNING: controller.log mentions 'rotation complete' {ledger_mentions} time(s) "
            "but 0 lines parsed as ledger entries. The log is most likely zap CONSOLE format "
            "(--zap-devel / chart value logging.development: true), whose trailing {...kv...} "
            "object has no \"msg\"/\"ts\" keys. The analyzer requires JSON logs — the Scenario P "
            "overlay (scenarios/controller-values.yaml) sets logging.development: false; "
            "re-check what the run was actually installed with."
        )
        print(ledger_warning, file=sys.stderr)
    rows, missing_claims = build_ledger_rows(ledger, births)
    margins_m = [fmt_min(r["margin_s"]) for r in rows]

    events, seq_ts, restarts, scrape_errors = scan_scrape_log(run)
    totals = counter_totals(events)

    # Criteria 1-3: counter sums. 1 and 2 are global (all pools) per the pinned
    # criteria list; 3 is explicitly main-pool-only (exact label match — the
    # epilogue pool "nodepool-soak-epi" is expected to fire exactly one forceful
    # fallback and must not be conflated with the main pool here).
    success = sum_counter(totals, "noderotation_completed_total", {"outcome": "success"})
    expired = sum_counter(totals, "noderotation_completed_total", {"outcome": "expired"})
    failure = sum_counter(totals, "noderotation_completed_total", {"outcome": "failure"})
    success_main = sum_counter(totals, "noderotation_completed_total", {"nodepool": MAIN_POOL, "outcome": "success"})
    ff_main = sum_counter(totals, "noderotation_forceful_fallback_total", {"nodepool": MAIN_POOL})

    # Criterion 4: short_lead_nodes must be 0 at every scrape, main pool only —
    # like the 7a gauges, this is a per-pool config/selection signal and the
    # epilogue pool's own reading (post T_end) must not contaminate it.
    short_lead = gauge_per_scrape(events, "noderotation_short_lead_nodes", {"nodepool": MAIN_POOL})
    short_lead_max = max(short_lead.values()) if short_lead else None

    # Criterion 5: scraper seq contiguity (per epoch) + restart coverage gaps
    # + SCRAPE_ERROR count. A gap of unknown duration (no bounding timestamp)
    # is treated as over-5m: it cannot be shown to satisfy the criterion.
    gaps = seq_gap_report(seq_ts)
    rgaps = restart_gaps(seq_ts, restarts)
    gaps_over_5m = [g for g in gaps + rgaps
                    if g["duration_s"] is None or g["duration_s"] > 300]

    # Criterion 7a: the six pinned gauges, main pool only, at every scrape where
    # each is present.
    gauge_mismatches = {}
    gauge_present = {}
    for gname, target in GAUGE_TARGETS.items():
        per_scrape = gauge_per_scrape(events, gname, {"nodepool": MAIN_POOL})
        gauge_present[gname] = len(per_scrape)
        bad = {seq: v for seq, v in per_scrape.items() if v != target}
        if bad:
            gauge_mismatches[gname] = bad

    # Criterion 9: end census from the LAST nodeclaims snapshot.
    last_snap = load_last_nodeclaims_snapshot(run)
    cen = census(run, last_snap)

    out = []
    out.append("# Scenario P analysis (issue #118)")
    out.append("")
    if ledger_warning:
        out.append(f"> **{ledger_warning}**")
        out.append("")
    out.append("## Per-rotation ledger")
    out.append("")
    out.append(f"rotations (ledger): {len(rows)}  (unmatched claims: {missing_claims or 'none'}, "
                f"malformed ts lines skipped: {ledger_bad})")
    out.append("")
    out.append("| claim | pool | mode | birth | completion | margin (min) |")
    out.append("|---|---|---|---|---|---|")
    for r in sorted(rows, key=lambda r: r["ts"]):
        out.append(
            f"| {r['claim']} | {r['pool']} | {r['mode'] or '-'} | "
            f"{r['birth'].isoformat()} | {r['ts'].isoformat()} | {fmt_min(r['margin_s']):.1f} |"
        )
    out.append("")

    out.append("## Margin distribution")
    out.append("")
    if margins_m:
        out.append(
            f"min={min(margins_m):.1f}m median={statistics.median(margins_m):.1f}m "
            f"max={max(margins_m):.1f}m (n={len(margins_m)})"
        )
    else:
        out.append("no rotations in ledger — no margin distribution")
    out.append("")

    out.append("## Counters")
    out.append("")
    out.append(f"success={success:.0f} (main-pool {success_main:.0f}) expired={expired:.0f} "
                f"failure={failure:.0f} main-pool forceful_fallback={ff_main:.0f}")
    out.append(f"scraper: SCRAPE_ERROR lines={scrape_errors}, seq gaps={len(gaps)}, "
                f"restarts={len(restarts)}"
               + (f" (>5m: {len(gaps_over_5m)})" if gaps or rgaps else ""))
    for g in gaps:
        dur = f"{g['duration_s']:.0f}s" if g["duration_s"] is not None else "unknown (no bounding seq)"
        out.append(f"  - epoch {g['epoch']}: seq {g['from']}-{g['to']} missing, "
                    f"bounded by {g['before_ts']} .. {g['after_ts']} ({dur})")
    for g in rgaps:
        dur = f"{g['duration_s']:.0f}s" if g["duration_s"] is not None else "unknown"
        out.append(f"  - scraper restart at {g['ts']} (seq {g['prev_seq']} -> {g['new_seq']}, "
                    f"coverage gap {g['before_ts']} .. {g['after_ts']}, {dur})")
    out.append("")

    out.append("## Criterion-4 short_lead_nodes (main pool)")
    out.append("")
    if short_lead:
        out.append(f"max={short_lead_max:.0f} across {len(short_lead)} scrapes")
    else:
        out.append("no short_lead_nodes readings for the main pool found")
    out.append("")

    out.append("## Criterion-7a gauges (main pool, pinned targets)")
    out.append("")
    out.append("| gauge | target | scrapes seen | mismatches |")
    out.append("|---|---|---|---|")
    for gname, target in GAUGE_TARGETS.items():
        n_bad = len(gauge_mismatches.get(gname, {}))
        out.append(f"| {gname} | {target:.0f} | {gauge_present[gname]} | {n_bad} |")
    out.append("")

    out.append("## Criterion-9 end census")
    out.append("")
    if cen["census_ts"] is None:
        out.append("no nodeclaims snapshot found — census not evaluable")
    else:
        out.append(f"census at {cen['census_ts'].isoformat()} "
                    f"(ageThreshold A={GAUGE_TARGETS['noderotation_age_threshold_seconds']:.0f}s)")
        if cen["old_no_inflight"]:
            out.append("flagged — older than A with no in-flight rotation:")
            for name, pool, age, state in cen["old_no_inflight"]:
                out.append(f"  - {name} (pool={pool}, age={age:.0f}s, state={state})")
        else:
            out.append("no claim older than A lacks an in-flight rotation")
        if cen["failed"]:
            out.append("flagged — state=failed:")
            for name, pool, age in cen["failed"]:
                out.append(f"  - {name} (pool={pool}, age={age:.0f}s)")
        else:
            out.append("no claim in state=failed")
        if cen["expired"]:
            out.append("informational — state=expired (evidences a criterion-1 violation):")
            for name, pool, age in cen["expired"]:
                out.append(f"  - {name} (pool={pool}, age={age:.0f}s)")
    out.append("")

    def verdict(cond, no_data=False):
        if no_data:
            return "FAIL (no data)"
        return "PASS" if cond else "FAIL"

    out.append("## PASS table (spec rev4 §6, offline-evaluable criteria)")
    out.append("")
    out.append("| criterion | value | verdict |")
    out.append("|---|---|---|")
    out.append(f"| 1 expired==0 | {expired:.0f} | {verdict(expired == 0)} |")
    out.append(f"| 2 success>=40 | {success:.0f} | {verdict(success >= 40)} |")
    out.append(f"| 3 main-pool fallback==0 | {ff_main:.0f} | {verdict(ff_main == 0)} |")
    out.append(
        f"| 4 short_lead max==0 (main pool) | "
        f"{'n/a' if short_lead_max is None else f'{short_lead_max:.0f}'} | "
        f"{verdict(short_lead_max == 0, no_data=short_lead_max is None)} |"
    )
    out.append(f"| 5 scraper seq contiguous | {len(gaps)} gap(s), {len(gaps_over_5m)} over 5m, "
                f"{len(restarts)} restart(s), {scrape_errors} SCRAPE_ERROR | "
                f"{verdict(len(gaps_over_5m) == 0)} |")
    out.append(
        f"| 7a six gauges match pinned values (main pool) | "
        f"{len(gauge_mismatches)}/{len(GAUGE_TARGETS)} series mismatched | "
        f"{verdict(not gauge_mismatches and all(gauge_present.values()), no_data=not all(gauge_present.values()))} |"
    )
    if margins_m:
        out.append(f"| 8 all margins>0 | min {min(margins_m):.1f}m | {verdict(min(margins_m) > 0)} |")
    else:
        out.append("| 8 all margins>0 | no rotations | FAIL |")
    census_ok = cen["census_ts"] is not None and not cen["old_no_inflight"] and not cen["failed"]
    out.append(
        f"| 9 end census clean | {len(cen['old_no_inflight'])} stale, {len(cen['failed'])} failed | "
        f"{verdict(census_ok, no_data=cen['census_ts'] is None)} |"
    )

    report = "\n".join(out) + "\n"
    with open(f"{run}/report.md", "w") as f:
        f.write(report)
    print(report)


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print("usage: soak-analyze.py <run-dir>", file=sys.stderr)
        sys.exit(2)
    main(sys.argv[1])
