#!/usr/bin/env python3
# Генератор monitoring/grafana/dashboards/kontakt-mesh.json — граф системы Kontakt Mesh.
#   python3 monitoring/grafana/gen_mesh_dashboard.py monitoring/grafana/dashboards/kontakt-mesh.json
import json, sys

DS = {"type": "prometheus", "uid": "prometheus"}
O = 'observer="$observer"'


def V(name, match=""):
    """Ряд kontakt_mesh_graph_<name> глазами выбранного узла, а если его нет — глазами Master'а.

    Список «Глазами узла» Grafana заполняет при открытии дашборда, а автообновление его не
    перечитывает: открыли до запуска mesh — переменная пустая (var-observer= в адресе), и граф
    навсегда показывал «mesh не запущен». Ушедший из mesh наблюдатель — то же самое.
    """
    m = "kontakt_mesh_graph_" + name
    return (f'({m}{{{O}{match}}} or on() '
            f'({m}{{{match.lstrip(",")}}} and on(observer) kontakt_mesh_observer_info{{title=~"master.*"}}))')

panels, pid, y = [], [0], [0]


def nid():
    pid[0] += 1
    return pid[0]


def row(title):
    panels.append({"type": "row", "title": title, "collapsed": False, "id": nid(),
                   "gridPos": {"x": 0, "y": y[0], "w": 24, "h": 1}, "panels": []})
    y[0] += 1


def tgt(expr, legend="__auto", ref="A", table=False):
    t = {"refId": ref, "datasource": DS, "expr": expr, "legendFormat": legend}
    if table:
        t.update({"format": "table", "instant": True, "range": False})
    else:
        t.update({"range": True, "instant": False})
    return t


def stat(title, desc, expr, x, w=4, color="blue", steps=None, unit="none"):
    panels.append({"type": "stat", "title": title, "description": desc, "id": nid(), "datasource": DS,
                   "gridPos": {"x": x, "y": y[0], "w": w, "h": 4}, "targets": [tgt(expr)],
                   "fieldConfig": {"defaults": {"unit": unit, "decimals": 0, "color": {"mode": "thresholds"},
                                                "thresholds": {"mode": "absolute", "steps": steps or [{"color": color, "value": None}]}},
                                   "overrides": []},
                   "options": {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
                               "colorMode": "background", "graphMode": "none", "textMode": "value", "justifyMode": "center"}})


def ts(title, desc, exprs, x, w=12, h=8, unit="none", vmax=None):
    panels.append({"type": "timeseries", "title": title, "description": desc, "id": nid(), "datasource": DS,
                   "gridPos": {"x": x, "y": y[0], "w": w, "h": h},
                   "targets": [dict(tgt(e, l), refId=chr(65 + i)) for i, (e, l) in enumerate(exprs)],
                   "fieldConfig": {"defaults": {"unit": unit, "min": 0, **({"max": vmax} if vmax is not None else {}), "custom": {"lineWidth": 1, "fillOpacity": 10, "showPoints": "never"}},
                                   "overrides": []},
                   "options": {"legend": {"displayMode": "table", "placement": "right", "calcs": ["lastNotNull", "max"]},
                               "tooltip": {"mode": "multi", "sort": "desc"}}})


BAD = [{"color": "green", "value": None}, {"color": "red", "value": 1}]
WARN = [{"color": "green", "value": None}, {"color": "orange", "value": 1}]

row("Сводка")
stat("Worker'ы HEALTHY", "Worker'ы в состоянии HEALTHY глазами наблюдателя (Master не считается).",
     "count(" + V("node", ',detail__health="HEALTHY",detail__role!="master"') + ") or vector(0)", 0, color="green")
stat("DEGRADED", "Узлы в DEGRADED: канал выше порога предупреждения или узел сам сообщил о деградации.",
     "count(" + V("node", ',detail__health="DEGRADED"') + ") or vector(0)", 4, steps=WARN)
stat("OFFLINE", "Узлы без heartbeat дольше TTL (у Master'а).",
     "count(" + V("node", ',detail__health="OFFLINE"') + ") or vector(0)", 8, steps=BAD)
stat("Рёбра в порядке", "Рёбра данных, качество которых проходит пороги маршрутизации.",
     "count(" + V("edge", ',detail__quality="ok"') + ") or vector(0)", 12, color="green")
stat("Плохие рёбра", "Рёбра warning или critical: маршрутизация их штрафует или обходит.",
     "count(" + V("edge", ',detail__quality=~"warning|critical"') + ") or vector(0)", 16, steps=WARN)
stat("Failover за час", "Сколько раз основной маршрут отказывал и заменялся (все узлы).",
     'sum(increase(kontakt_route_failovers_total[1h])) or vector(0)', 20, steps=WARN)
y[0] += 4

# Пустой ответ Node Graph не переносит (Grafana 11, nodeGraph/utils.ts): пустой кадр рёбер
# без поля source он принимает за кадр узлов и падает «id field is required for nodes», а
# ребро к несуществующему узлу роняет раскладку. Поэтому:
#   - узлов нет (mesh не запущен) — один узел-подсказка «mesh не запущен»;
#   - рёбер нет (mesh не запущен или узел один) — петля на один из СУЩЕСТВУЮЩИХ узлов: её не видно,
#     но кадр рёбер есть. `or on()` добавляет заглушку только при пустой левой части.
NO_MESH = ('label_replace(label_replace(label_replace(vector(0), "id", "no-mesh", "", ""), '
           '"title", "mesh не запущен", "", ""), "subtitle", "./scripts/mesh-demo.sh", "", "")')
NODES_Q = f'{V("node")} or on() {NO_MESH}'
EDGES_Q = (f'{V("edge")} or on() max by (id, source, target) ('
           f'label_replace(label_replace(label_replace(topk(1, {V("node")} or on() {NO_MESH}), '
           '"source", "$1", "id", "(.*)"), "target", "$1", "id", "(.*)"), "id", "idle", "", ""))')

row("Граф системы")
panels.append({
    "type": "nodeGraph", "title": "Kontakt Mesh — граф системы", "id": nid(), "datasource": DS,
    "description": "Узлы и рёбра глазами выбранного наблюдателя (Master видит всю систему по heartbeat'ам, "
                   "Worker — свою окрестность). Цвет узла — здоровье, синий — Master. Ребро: зелёное — в порядке, "
                   "оранжевое — выше порога предупреждения, красное — выше критического (маршрутизация его обходит), "
                   "серое пунктиром — не подтверждено второй стороной или ещё не измерено, синее пунктиром — control "
                   "plane к Master'у (голос по нему не ходит), красное пунктиром — heartbeat пропал (узел OFFLINE). На узле — активные звонки, на ребре (при наведении) — RTT. Стрелки направления не означают: ребро общее для обоих концов.",
    "gridPos": {"x": 0, "y": y[0], "w": 24, "h": 26},
    "targets": [tgt(NODES_Q, ref="nodes", table=True), tgt(EDGES_Q, ref="edges", table=True)],
    # Node Graph ищет поле по НАСТОЯЩЕМУ имени mainstat, а переименование в Grafana
    # меняет только отображаемое имя. Поэтому поле создаётся заново (calculateField с
    # alias), а исходный столбец значения («Value #nodes» / «Value #edges») убирается.
    "transformations": [
        {"id": "calculateField", "options": {"mode": "reduceRow", "reduce": {"reducer": "lastNotNull"},
                                             "alias": "mainstat", "replaceFields": False,
                                             # иначе Grafana склеит кадры узлов и рёбер по времени в один
                                             "timeSeries": False}},
        {"id": "organize", "options": {"excludeByName": {
            "Time": True, "__name__": True, "job": True, "instance": True, "observer": True,
            "Value": True, "Value #nodes": True, "Value #edges": True}}},
        # у узла — целое число звонков: число Node Graph всегда пишет с двумя знаками
        {"id": "convertFieldType", "filter": {"id": "byRefId", "options": "nodes"},
         "options": {"conversions": [{"targetField": "mainstat", "destinationType": "string"}]}}],
    "options": {"nodes": {"mainStatUnit": ""}, "edges": {"mainStatUnit": "ms"}},
})
y[0] += 26

row("Узлы и рёбра")
panels.append({
    "type": "table", "title": "Узлы", "id": nid(), "datasource": DS,
    "description": "По узлам графа: звонки, свободно (по замерам), загрузка канала (пусто — ёмкость неизвестна), CPU, память, давность heartbeat.",
    "gridPos": {"x": 0, "y": y[0], "w": 24, "h": 9},
    "targets": [tgt(V("node_stat"), table=True)],
    "transformations": [{"id": "groupingToMatrix", "options": {"columnField": "stat", "rowField": "title", "valueField": "Value"}}],
    "fieldConfig": {"defaults": {"decimals": 2}, "overrides": []},
})
panels.append({
    "type": "table", "title": "Рёбра", "id": nid(), "datasource": DS,
    "description": "Рёбра графа: тип пути, качество по порогам маршрутизации, потери, джиттер, RTT (мс).",
    "gridPos": {"x": 0, "y": y[0] + 9, "w": 24, "h": 10},
    "targets": [tgt(V("edge"), table=True)],
    "transformations": [{"id": "organize", "options": {
        "excludeByName": {"Time": True, "__name__": True, "job": True, "instance": True, "observer": True, "id": True,
                          "color": True, "strokeDasharray": True, "source": True, "target": True},
        "indexByName": {"detail__from": 0, "detail__to": 1, "detail__kind": 2, "detail__quality": 3,
                        "detail__loss": 4, "detail__jitter": 5, "Value": 6},
        "renameByName": {"Value": "RTT, мс", "detail__from": "от", "detail__to": "к", "detail__kind": "путь",
                         "detail__quality": "качество", "detail__loss": "потери", "detail__jitter": "джиттер"}}}],
    "fieldConfig": {"defaults": {"decimals": 2}, "overrides": []},
})
y[0] += 19

row("Качество рёбер во времени")
ts("RTT рёбер", "Сглаженный RTT каждого ребра, как его меряет каждый узел.",
   [('kontakt_rtt_seconds', "{{instance}} → {{peer}}")], 0, unit="s")
ts("Потери на рёбрах", "Доля потерянных проб в окне; ≥5% — ребро непригодно для маршрута.",
   [('kontakt_link_loss', "{{instance}} → {{peer}}")], 12, unit="percentunit", vmax=1)
y[0] += 8

dash = {"uid": "kontakt-mesh", "title": "Kontakt Mesh", "tags": ["kontakt", "mesh"],
        "description": "Граф системы Kontakt Mesh: Master, Worker'ы, рёбра и их качество.",
        "timezone": "browser", "schemaVersion": 39, "version": 1, "refresh": "10s", "editable": True,
        "time": {"from": "now-30m", "to": "now"},
        "templating": {"list": [{
            "name": "observer", "label": "Глазами узла", "type": "query", "datasource": DS,
            "query": {"query": "query_result(kontakt_mesh_observer_info)", "refId": "A"},
            "regex": '/observer="(?<value>[^"]+)".*title="(?<text>[^"]+)"/', "refresh": 2, "sort": 1,
            "includeAll": False, "multi": False}]},
        "annotations": {"list": []}, "panels": panels}
json.dump(dash, open(sys.argv[1], "w", encoding="utf-8"), ensure_ascii=False, indent=1)
print(len(panels), "панелей")
