#!/usr/bin/env python3
# Генератор monitoring/grafana/dashboards/kontakt-radio.json — дашборд «Открытого радио».
# Правится здесь, а не в JSON руками:
#   python3 monitoring/grafana/gen_radio_dashboard.py monitoring/grafana/dashboards/kontakt-radio.json
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
def ts(title, desc, exprs, x, w=12, h=8, unit="none", stack=False, maxv=None):
    d = {"unit": unit, "min": 0, "custom": {"lineWidth": 1, "fillOpacity": 15,
         "showPoints": "never", "stacking": {"mode": "normal" if stack else "none", "group": "A"}}}
    if maxv is not None:
        d["max"] = maxv
    panels.append({"type": "timeseries", "title": title, "description": desc, "id": nid(), "datasource": DS,
        "gridPos": {"x": x, "y": y[0], "w": w, "h": h},
        "targets": [tgt(e, l, i) for i, (e, l) in enumerate(exprs)],
        "fieldConfig": {"defaults": d, "overrides": []},
        "options": {"legend": {"displayMode": "table", "placement": "bottom", "calcs": ["lastNotNull", "max"]},
                    "tooltip": {"mode": "multi", "sort": "desc"}}})

W = "$window"
I = 'instance=~"$instance"'
row("Сейчас")
stat("Радио", "Отвечает ли сервер радио: Prometheus забирает /metrics.",
     f'min(up{{job="radio",{I}}}) or vector(0)', 0,
     steps=[{"color": "red", "value": None}, {"color": "green", "value": 1}])
stat("В эфире", "Станции, у которых сейчас подключён ведущий.",
     f'sum(kontakt_radio_stations{{{I}}}) or vector(0)', 4, color="orange")
stat("Приёмники", "Открытые страницы радио: и настроенные на станцию, и слушающие шорох.",
     f'sum(kontakt_radio_listeners{{{I}}}) or vector(0)', 8)
stat("Слушают станции", "Приёмники, ручка которых стоит на станции в эфире — только им идёт звук.",
     f'sum(kontakt_radio_listeners_tuned{{{I}}}) or vector(0)', 12, color="green")
stat("Отдаётся эфира", "Звук слушателям по факту: байты × 8. 64 кбит/с на каждого настроенного слушателя.",
     f'sum(rate(kontakt_radio_bytes_out_total{{{I}}}[{W}])) * 8 or vector(0)', 16, unit="bps", dec=1)
stat("Потери у слушателей", "Доля кадров, не влезших в очередь медленного слушателя. Больше 5% — слышны обрывы.",
     f'(sum(rate(kontakt_radio_frames_dropped_total{{reason="slow_listener",{I}}}[{W}]))'
     f' / clamp_min(sum(rate(kontakt_radio_frames_out_total{{{I}}}[{W}])) + sum(rate(kontakt_radio_frames_dropped_total{{reason="slow_listener",{I}}}[{W}])), 1))'
     ' or vector(0)', 20, unit="percentunit", dec=1,
     steps=[{"color": "green", "value": None}, {"color": "yellow", "value": 0.01}, {"color": "red", "value": 0.05}])
y[0] += 4

row("Эфир")
ts("Станции и слушатели", "Сколько станций в эфире, сколько приёмников открыто и сколько из них на станции.",
   [(f'sum(kontakt_radio_stations{{{I}}})', "станций"),
    (f'sum(kontakt_radio_listeners{{{I}}})', "приёмников"),
    (f'sum(kontakt_radio_listeners_tuned{{{I}}})', "на станции")], 0, 12)
ts("Слушатели по волнам", "По частоте, без названий: названия задают люди, в метриках им не место.",
   [(f'sum by (freq) (kontakt_radio_station_listeners{{{I}}})', "{{freq}} FM")], 12, 12, stack=True)
y[0] += 8
ts("Поток от ведущих, кадров/с", "Нормально — 50 кадров в секунду на станцию (20 мс G.711). Провал — у ведущего "
   "вкладка ушла в фон или плохая связь: слушатели слышат обрывы.",
   [(f'sum(rate(kontakt_radio_frames_in_total{{{I}}}[{W}]))', "пришло"),
    (f'sum(kontakt_radio_stations{{{I}}}) * 50', "норма (50 × станций)")], 0, 12, unit="short")
ts("Полоса", "Сколько звука сервер принимает от ведущих и раздаёт слушателям, и служебный трафик — списки "
   "станций и сообщения о треке приёмникам. Служебный должен быть долями мегабита: если он растёт вместе с "
   "числом слушателей, а не с выходом станций, — список снова рассылается не по делу (LOAD_REPORT.md).",
   [(f'sum(rate(kontakt_radio_bytes_in_total{{{I}}}[{W}])) * 8', "от ведущих"),
    (f'sum(rate(kontakt_radio_bytes_out_total{{{I}}}[{W}])) * 8', "слушателям"),
    (f'sum(rate(kontakt_radio_list_bytes_total{{{I}}}[{W}])) * 8 or vector(0)', "списки станций (служебное)")],
   12, 12, unit="bps")
y[0] += 8

row("Качество и отказы")
ts("Потерянные кадры", "slow_listener — слушатель не успевает принимать (его сеть или канал сервера); "
   "host_rate — ведущий шлёт больше 64 кбит/с, лишнее отброшено.",
   [(f'sum by (reason) (rate(kontakt_radio_frames_dropped_total{{{I}}}[{W}]))', "{{reason}}")], 0, 8, unit="short")
ts("Отказы", "busy — волна занята; bad_freq — вне 87.5–108.0; full / listeners_full — достигнут лимит станций / приёмников.",
   [(f'sum by (reason) (increase(kontakt_radio_rejected_total{{{I}}}[{W}]))', "{{reason}}")], 8, 8, unit="short")
ts("Перезапуски", "Время запуска сервера: ступенька — обновление (radio.sh update) или подъём после падения.",
   [(f'max(kontakt_radio_start_time_seconds{{{I}}}) * 1000', "запущен")], 16, 8, unit="dateTimeAsIso")
y[0] += 8

row("Люди и ссылки")
ts("Подключения", "Выходы станций в эфир, подключения приёмников, перенастройки ручки — за окно.",
   [(f'sum(increase(kontakt_radio_host_sessions_total{{{I}}}[{W}]))', "станций вышло в эфир"),
    (f'sum(increase(kontakt_radio_listener_sessions_total{{{I}}}[{W}]))', "приёмников подключилось"),
    (f'sum(increase(kontakt_radio_tunes_total{{{I}}}[{W}]))', "перенастроек ручки")], 0, 8, unit="short")
ts("Ссылки на волну", "page — открыли ссылку (люди и мессенджеры, которые строят карточку); image — запросили картинку "
   "превью; нарисовано — промахи кеша.",
   [(f'sum by (kind) (increase(kontakt_radio_share_views_total{{{I}}}[{W}]))', "{{kind}}"),
    (f'sum(increase(kontakt_radio_og_renders_total{{{I}}}[{W}]))', "нарисовано")], 8, 8, unit="short")
ts("Сколько длится эфир", "Медиана и 90-й процентиль времени в эфире у станций, закончивших вещание за последний час.",
   [(f'histogram_quantile({q}, sum by (le) (increase(kontakt_radio_on_air_seconds_bucket{{{I}}}[1h])))', f"p{int(q*100)}")
    for q in (0.5, 0.9)], 16, 8, unit="s")
y[0] += 8

panels.append({"type": "alertlist", "title": "Тревоги радио", "id": nid(),
    "description": "Правила — monitoring/prometheus/rules/radio.yml.",
    "gridPos": {"x": 0, "y": y[0], "w": 24, "h": 6},
    "options": {"alertListOptions": {}, "showOptions": "current", "maxItems": 20, "sortOrder": 1,
                "dashboardAlerts": False, "alertName": "Radio", "alertInstanceLabelFilter": "",
                "stateFilter": {"firing": True, "pending": True, "noData": False, "normal": False, "error": True},
                "viewMode": "list", "groupMode": "default", "datasource": "prometheus"}})
y[0] += 6

dash = {"uid": "kontakt-radio", "title": "Открытое радио", "tags": ["kontakt", "radio"],
  "description": "Станции, слушатели, поток от ведущих, потери и отказы, ссылки на волну.",
  "timezone": "browser", "schemaVersion": 39, "version": 1, "refresh": "10s", "graphTooltip": 1, "editable": True,
  "time": {"from": "now-3h", "to": "now"},
  "templating": {"list": [
    {"name": "instance", "label": "Сервер", "type": "query", "datasource": DS,
     "query": {"query": 'label_values(up{job="radio"}, instance)', "refId": "A"}, "refresh": 2,
     "includeAll": True, "allValue": ".*", "multi": True, "current": {"text": "All", "value": "$__all"}, "sort": 1},
    {"name": "window", "label": "Окно", "type": "custom", "query": "1m,5m,15m",
     "current": {"text": "1m", "value": "1m"},
     "options": [{"text": v, "value": v, "selected": v == "1m"} for v in ("1m", "5m", "15m")]}]},
  "annotations": {"list": [{"builtIn": 1, "datasource": {"type": "grafana", "uid": "-- Grafana --"}, "enable": True,
     "hide": True, "iconColor": "rgba(0, 211, 255, 1)", "name": "Annotations & Alerts", "type": "dashboard"}]},
  "panels": panels}
json.dump(dash, open(sys.argv[1], "w", encoding="utf-8"), ensure_ascii=False, indent=1)
print(len(panels), "панелей")
