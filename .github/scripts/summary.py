#!/usr/bin/env python3
"""Markdown для страницы прогона CI ($GITHUB_STEP_SUMMARY), по образцу xk6-sip.

  summary.py functional [--all-steps] <папка отчётов> <сценарий>...   из JUnit-отчётов
      шаги только проваленных сценариев; --all-steps добавляет прошедшие, свёрнутыми
  summary.py load <summary.json> <заголовок>                          из k6 --summary-export
  summary.py gosec <gosec.json>                                       замечания gosec по правилам
"""
import collections
import json
import os
import sys
import xml.etree.ElementTree as ET


def cell(s):
    return str(s).replace("|", "\\|")


def functional(reports, scenarios, all_steps):
    rows, details = [], []
    for s in scenarios:
        path = os.path.join(reports, f"{s}.xml")
        if not os.path.exists(path):
            rows.append(f"| {s} | ❌ | — | нет отчёта: k6 остановился, не записав его |")
            continue
        cases = [(c.get("name"), c.find("failure") is None) for c in ET.parse(path).iter("testcase")]
        steps = [c for c in cases if not c[0].startswith("threshold ")]
        failed = [name for name, ok in cases if not ok]
        passed = sum(ok for _, ok in steps)
        ok = not failed
        # проваленный шаг останавливает сценарий, поэтому он и есть первый провал
        first = next((n for n, good in steps if not good), failed[0] if failed else "")
        count = f"{len(steps)}" if ok else f"{passed} из {len(steps)}"
        rows.append(f"| {s} | {'✅' if ok else '❌'} | {count} | {cell(first)} |")
        table = "\n".join(f"| {'✅' if good else '❌'} | {cell(n)} |" for n, good in cases)
        steps_md = f"| | Шаг |\n|---|---|\n{table}\n"
        if all_steps:
            details.append(f"<details{'' if ok else ' open'}><summary>{'✅' if ok else '❌'} {s}</summary>\n\n"
                           f"{steps_md}\n</details>\n")
        elif not ok:
            details.append(f"#### ❌ {s}\n\n{steps_md}")
    bad = sum("| ❌ |" in r for r in rows)
    head = (f"### ❌ Звонки (xk6-sip): провалено {bad} из {len(rows)} сценариев" if bad
            else f"### ✅ Звонки (xk6-sip): {len(rows)} сценариев прошли")
    print(f"{head}\n\n| Сценарий | Итог | Шаги | Проваленный шаг |\n|---|---|---|---|")
    print("\n".join(rows) + "\n")
    print("\n".join(details))


def ms(v):
    return f"{v:.2f} мс" if v < 10 else f"{v:.0f} мс"


def pct(v):
    return f"{v * 100:.2f}".rstrip("0").rstrip(".") + "%"


def load(path, title):
    m = json.load(open(path, encoding="utf-8"))["metrics"]
    get = lambda name, key, default=None: m.get(name, {}).get(key, default)
    shown, rows = set(), []

    def row(label, value, metric=None):
        th = m.get(metric, {}).get("thresholds", {}) if metric else {}
        shown.add(metric)
        mark = ("❌" if any(th.values()) else "✅") if th else ""  # summary-export: true — порог пробит
        rows.append(f"| {label} | {value} | {cell(', '.join(th)) or '—'} | {mark} |")

    calls = get("sip_call_success", "passes", 0) + get("sip_call_success", "fails", 0)
    row("Звонков", f"{calls}, {get('sip_call_results', 'rate', 0):.2f} в секунду")
    if "sip_call_success" in m:
        row("Станция сняла трубку (200 OK)", pct(get("sip_call_success", "value")), "sip_call_success")
    if "sip_call_setup_time" in m:
        row("От INVITE до 200 OK, p95", ms(get("sip_call_setup_time", "p(95)")), "sip_call_setup_time")
    if "rtp_audio_heard" in m:
        row("В трубке был звук", pct(get("rtp_audio_heard", "value")), "rtp_audio_heard")
    if "kontakt_peer_heard" in m:
        row("Слышали собеседника, а не только гудки", pct(get("kontakt_peer_heard", "value")), "kontakt_peer_heard")
    if "rtp_audio_score" in m:
        row("Качество звука p50 / min",
            f"{get('rtp_audio_score', 'med'):.3f} / {get('rtp_audio_score', 'min'):.3f}", "rtp_audio_score")
    if "rtp_jitter" in m:
        row("Джиттер p95", ms(get("rtp_jitter", "p(95)")), "rtp_jitter")
    if "rtp_packets_received" in m:
        row("RTP получено / потеряно",
            f"{get('rtp_packets_received', 'count', 0):,} / {get('rtp_packets_lost', 'count', 0):,}")
    if "checks" in m:
        row("Проверки", f"{get('checks', 'passes', 0):,} из {get('checks', 'passes', 0) + get('checks', 'fails', 0):,}",
            "checks")
    for metric, v in m.items():  # пороги на всём, что не перечислено выше
        if v.get("thresholds") and metric not in shown:
            row(metric, "", metric)

    ths = [crossed for v in m.values() for crossed in v.get("thresholds", {}).values()]
    bad = sum(ths)
    head = (f"### ❌ Нагрузка (xk6-sip): пробито {bad} из {len(ths)} порогов" if bad
            else f"### ✅ Нагрузка (xk6-sip): {len(ths)} порогов соблюдены")
    print(f"{head}\n\n{title}\n\n| Метрика | Значение | Порог | |\n|---|---|---|---|")
    print("\n".join(rows))
    print("\nДашборд прогона целиком — `dashboard.png` в артефакте **load-report**.\n")


def gosec(path):
    issues = json.load(open(path, encoding="utf-8")).get("Issues") or []
    by_rule = collections.Counter(i["rule_id"] for i in issues)
    what = {i["rule_id"]: i["details"].split(" conversion ")[0] for i in issues}
    print(f"### gosec: {len(issues)} замечаний (отчёт, сборку не останавливает)\n")
    print("| Правило | Сколько | Что |\n|---|---|---|")
    for rule, n in by_rule.most_common():
        print(f"| {rule} | {n} | {cell(what[rule])} |")
    print("\nПолный список — `gosec.json` в артефакте **gosec-report**.\n")


if __name__ == "__main__":
    if len(sys.argv) >= 3 and sys.argv[1] == "functional":
        args = [a for a in sys.argv[2:] if a != "--all-steps"]
        functional(args[0], args[1:], "--all-steps" in sys.argv)
    elif len(sys.argv) == 4 and sys.argv[1] == "load":
        load(sys.argv[2], sys.argv[3])
    elif len(sys.argv) == 3 and sys.argv[1] == "gosec":
        gosec(sys.argv[2])
    else:
        sys.exit(__doc__)
