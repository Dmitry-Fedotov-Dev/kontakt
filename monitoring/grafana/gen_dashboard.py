#!/usr/bin/env python3
# Генератор monitoring/grafana/dashboards/kontakt.json. Дашборд правится здесь,
# а не в JSON руками:  python3 monitoring/grafana/gen_dashboard.py monitoring/grafana/dashboards/kontakt.json
import json, sys
DS = {"type": "prometheus", "uid": "prometheus"}
panels, pid, y = [], [0], [0]
def nid():
    pid[0] += 1; return pid[0]
def row(title):
    panels.append({"type": "row", "title": title, "collapsed": False, "id": nid(),
                   "gridPos": {"x": 0, "y": y[0], "w": 24, "h": 1}, "panels": []}); y[0] += 1
def tgt(expr, legend="__auto", i=0):
    return {"refId": chr(65 + i), "datasource": DS, "expr": expr, "legendFormat": legend, "range": True, "instant": False}
def stat(title, desc, expr, x, w=4, unit="none", color="blue", steps=None, dec=0):
    panels.append({"type": "stat", "title": title, "description": desc, "id": nid(), "datasource": DS,
        "gridPos": {"x": x, "y": y[0], "w": w, "h": 4}, "targets": [tgt(expr)],
        "fieldConfig": {"defaults": {"unit": unit, "decimals": dec, "color": {"mode": "thresholds"},
            "thresholds": {"mode": "absolute", "steps": steps or [{"color": color, "value": None}]}}, "overrides": []},
        "options": {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
                    "colorMode": "background", "graphMode": "area", "textMode": "value", "justifyMode": "center"}})
def ts(title, desc, exprs, x, w=12, h=8, unit="none", stack=False):
    panels.append({"type": "timeseries", "title": title, "description": desc, "id": nid(), "datasource": DS,
        "gridPos": {"x": x, "y": y[0], "w": w, "h": h},
        "targets": [tgt(e, l, i) for i, (e, l) in enumerate(exprs)],
        "fieldConfig": {"defaults": {"unit": unit, "min": 0, "custom": {"lineWidth": 1, "fillOpacity": 15,
            "showPoints": "never", "stacking": {"mode": "normal" if stack else "none", "group": "A"}}}, "overrides": []},
        "options": {"legend": {"displayMode": "table", "placement": "bottom", "calcs": ["lastNotNull", "max"]},
                    "tooltip": {"mode": "multi", "sort": "desc"}}})

W = "$window"
row("Обзор")
stat("На станции", "Ноги на станции: в очереди и в разговоре.",
     'sum(kontakt_signal_legs) or vector(0)', 0)
stat("Говорят", "Абоненты в разговоре.",
     'sum(kontakt_signal_legs{state="talking"}) or vector(0)', 4, color="green")
stat("В очереди", "Сколько абонентов ждут пару.",
     'max(kontakt_queue_length) or vector(0)', 8,
     steps=[{"color": "green", "value": None}, {"color": "yellow", "value": 10}, {"color": "red", "value": 50}])
stat("Ожидание p95", "Время в очереди до соединения, 95-й процентиль.",
     f'histogram_quantile(0.95, sum by (le) (rate(kontakt_signal_queue_wait_seconds_bucket[{W}])))', 12, unit="s", dec=2,
     steps=[{"color": "green", "value": None}, {"color": "yellow", "value": 10}, {"color": "red", "value": 30}])
stat("Полоса RTP", "Трафик медиа-узла по факту, обе стороны (байты RTP × 8).",
     f'sum(rate(kontakt_media_bytes_in_total[{W}]) + rate(kontakt_media_bytes_out_total[{W}])) * 8 or vector(0)',
     16, unit="bps", dec=1)
stat("Отказы", "Звонки, не снятые станцией, за окно (любая причина, кроме accepted).",
     f'sum(increase(kontakt_signal_calls_total{{result!="accepted"}}[{W}])) or vector(0)', 20,
     steps=[{"color": "green", "value": None}, {"color": "red", "value": 1}])
y[0] += 4

row("Сигнализация (signal)")
ts("Звонки по исходу", "INVITE в секунду: accepted или причина отказа.",
   [(f'sum by (result) (rate(kontakt_signal_calls_total[{W}]))', "{{result}}")], 0, 8, unit="reqps")
ts("Пары и отбои", "Соединённые пары и отбои по причине в секунду.",
   [(f'sum(rate(kontakt_pairs_total[{W}]))', "пары"),
    (f'sum by (reason) (rate(kontakt_signal_hangups_total[{W}]))', "отбой {{reason}}")], 8, 8, unit="ops")
ts("Ожидание собеседника", "Время в очереди до соединения: p50 / p95 / p99.",
   [(f'histogram_quantile({q}, sum by (le) (rate(kontakt_signal_queue_wait_seconds_bucket[{W}])))', f"p{int(q*100)}")
    for q in (0.5, 0.95, 0.99)], 16, 8, unit="s")
y[0] += 8
ts("Ноги по состоянию", "setup / queued / talking.",
   [('sum by (state) (kontakt_signal_legs)', "{{state}}")], 0, 12, stack=True)
ts("Очередь", "Сколько абонентов ждут пару.",
   [('max(kontakt_queue_length)', "в очереди")], 12, 12)
y[0] += 8

row("Медиа (media)")
ts("Каналы", "Точки по транспорту, мосты, слушающие гудки или шум.",
   [('sum by (transport) (kontakt_media_endpoints)', "точки {{transport}}"),
    ('sum(kontakt_media_bridges)', "мосты"),
    ('sum(kontakt_media_tone_endpoints)', "гудки/шум")], 0, 8)
ts("RTP, пакетов/с", "Принято от устройств и отправлено устройствам.",
   [(f'sum(rate(kontakt_media_packets_in_total[{W}]))', "in"),
    (f'sum(rate(kontakt_media_packets_out_total[{W}]))', "out")], 8, 8, unit="pps")
ts("Полоса по факту", "Байты RTP × 8: из этого, а не из константы, считается ёмкость узла.",
   [(f'sum(rate(kontakt_media_bytes_in_total[{W}])) * 8', "in"),
    (f'sum(rate(kontakt_media_bytes_out_total[{W}])) * 8', "out"),
    # На канал, а не на разговор: гудки ждущим тоже трафик, и в начале и конце прогона
    # мостов ноль — делитель «мосты» давал мегабиты на разговор. Делитель — среднее
    # число точек за то же окно, что и rate(): иначе на обрыве нагрузки точек уже 0,
    # а rate() ещё нет.
    (f'sum(rate(kontakt_media_bytes_in_total[{W}]) + rate(kontakt_media_bytes_out_total[{W}])) * 8 / sum(avg_over_time(kontakt_media_endpoints[{W}]))', "на канал")],
   16, 8, unit="bps")
y[0] += 8
ts("Отказы в точке", "rtp_ports — кончились RTP-порты.",
   [(f'sum by (reason) (rate(kontakt_media_rejected_total[{W}]))', "{{reason}}")], 0, 12, unit="ops")
ts("Новые точки", "Созданные медиа-точки по транспорту в секунду.",
   [(f'sum by (transport) (rate(kontakt_media_endpoints_created_total[{W}]))', "{{transport}}")], 12, 12, unit="ops")
y[0] += 8

row("Абоненты (xk6-sip)")
T = 'testid=~"$testid"'
stat("Звонков/с", "Новые звонки абонентов k6 в секунду.", f'sum(rate(k6_sip_call_results_total{{{T}}}[{W}])) or vector(0)', 0, dec=1)
stat("Трубку сняли", "Доля звонков, на которые станция ответила 200 OK.",
     f'sum(rate(k6_sip_call_results_total{{{T},result="success"}}[{W}])) / sum(rate(k6_sip_call_results_total{{{T}}}[{W}]))',
     4, unit="percentunit", dec=2, steps=[{"color": "red", "value": None}, {"color": "yellow", "value": 0.95}, {"color": "green", "value": 0.99}])
stat("Ответ станции p95", "От INVITE до 200 OK.",
     f'histogram_quantile(0.95, sum(rate(k6_sip_call_setup_time_seconds{{{T}}}[{W}])))', 8, unit="s", dec=3,
     steps=[{"color": "green", "value": None}, {"color": "yellow", "value": 0.3}, {"color": "red", "value": 1}])
stat("Слышат собеседника", "Доля записанных звонков, где абонент услышал фразу собеседника, а не только гудки (kontakt_peer_heard).",
     f'avg(k6_kontakt_peer_heard_rate{{{T}}})', 12, unit="percentunit", dec=1,
     steps=[{"color": "red", "value": None}, {"color": "yellow", "value": 0.9}, {"color": "green", "value": 0.98}])
stat("Тишина в трубке", "Ноги, где за весь звонок не пришло звука: ни гудков, ни собеседника.",
     f'(sum(rate(k6_rtp_legs_total{{{T},heard="false"}}[{W}])) or vector(0)) / sum(rate(k6_rtp_legs_total{{{T}}}[{W}]))',
     16, unit="percentunit", dec=2, steps=[{"color": "green", "value": None}, {"color": "red", "value": 0.001}])
stat("Качество звука p50", "Оценка compareAudio() услышанной фразы, 0..1.",
     f'1 - histogram_quantile(0.5, sum(rate(k6_rtp_audio_mismatch{{{T}}}[{W}])))', 20, dec=3,
     steps=[{"color": "red", "value": None}, {"color": "yellow", "value": 0.8}, {"color": "green", "value": 0.9}])
y[0] += 4
ts("Звонки абонентов по исходу", "success / failure / cancelled в секунду.",
   [(f'sum by (result) (rate(k6_sip_call_results_total{{{T}}}[{W}]))', "{{result}}")], 0, 8, unit="ops")
ts("Ответ станции", "От INVITE до 200 OK: p50 / p95 / p99.",
   [(f'histogram_quantile({q}, sum(rate(k6_sip_call_setup_time_seconds{{{T}}}[{W}])))', f"p{int(q*100)}") for q in (0.5, 0.95, 0.99)],
   8, 8, unit="s")
ts("Джиттер и потери RTP", "Джиттер p95 по направлениям; доля потерянных пакетов.",
   [(f'histogram_quantile(0.95, sum by (direction) (rate(k6_rtp_jitter_seconds{{{T}}}[{W}])))', "джиттер {{direction}}"),
    (f'sum(rate(k6_rtp_packets_lost_total{{{T}}}[{W}])) / clamp_min(sum(rate(k6_rtp_packets_received_total{{{T}}}[{W}])), 1)', "потери")],
   16, 8)
y[0] += 8

row("Машина (профиль linux-host)")
ts("CPU машины", "Загрузка всех ядер (node-exporter).",
   [(f'1 - avg(rate(node_cpu_seconds_total{{mode="idle"}}[{W}]))', "занято")], 0, 8, unit="percentunit")
ts("Память машины", "Использовано и доступно.",
   [('node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes', "использовано"),
    ('node_memory_MemAvailable_bytes', "доступно")], 8, 8, unit="bytes")
ts("Сеть машины", "Входящий и исходящий трафик (без lo).",
   [(f'sum(rate(node_network_receive_bytes_total{{device!="lo"}}[{W}])) * 8', "in"),
    (f'sum(rate(node_network_transmit_bytes_total{{device!="lo"}}[{W}])) * 8', "out")], 16, 8, unit="bps")
y[0] += 8

dash = {"uid": "kontakt", "title": "Контакт: станция", "tags": ["kontakt", "sip", "rtp"],
  "description": "Абоненты, очередь, сигнализация и медиа станции «Контакт»; при нагрузке xk6-sip — сторона абонентов.",
  "timezone": "browser", "schemaVersion": 39, "version": 1, "refresh": "10s", "graphTooltip": 1, "editable": True,
  "time": {"from": "now-30m", "to": "now"},
  "templating": {"list": [{"name": "testid", "label": "Прогон k6", "type": "query", "datasource": DS,
     "query": {"query": "label_values(k6_vus, testid)", "refId": "A"}, "refresh": 2, "includeAll": True, "allValue": ".*",
     "multi": True, "current": {"text": "All", "value": "$__all"}, "sort": 2},
    {"name": "window", "label": "Окно", "type": "custom", "query": "30s,1m,5m",
     "current": {"text": "1m", "value": "1m"},
     "options": [{"text": v, "value": v, "selected": v == "1m"} for v in ("30s", "1m", "5m")]}]},
  "annotations": {"list": [{"builtIn": 1, "datasource": {"type": "grafana", "uid": "-- Grafana --"}, "enable": True,
     "hide": True, "iconColor": "rgba(0, 211, 255, 1)", "name": "Annotations & Alerts", "type": "dashboard"}]},
  "panels": panels}
json.dump(dash, open(sys.argv[1], "w", encoding="utf-8"), ensure_ascii=False, indent=1)
print(len(panels), "панелей")
