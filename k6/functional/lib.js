// Общая часть функциональных сценариев «Контакта» на xk6-sip: один VU, одна
// итерация, любой проваленный шаг останавливает сценарий (k6 выходит с ненулевым
// кодом) и JUnit-отчёт для CI (-e JUNIT=report.xml) — по тест-кейсу на шаг.
//
// Абоненты — SIP-софтфоны по UDP без регистрации: так к станции и ходят
// Linphone/MicroSIP. Станция опознаёт человека по «IP + имя из From», поэтому
// устройства A, B, C с одной машины — три разных человека.
//
//   IP=127.0.0.1 SIP_UDP=:5060 ./scripts/run.sh
//   k6 run -e JUNIT=pair.xml k6/functional/pair.js
//   k6 run -e NODES=10.0.0.4:5060,10.0.0.5:5060 …  # несколько входов станции (по кругу)
import sip from 'k6/x/sip';
import { check, fail } from 'k6';

export const options = {
  vus: 1,
  iterations: 1,
  thresholds: { checks: ['rate==1'] },
};

const env = (k, d) => __ENV[k] || d;
export const nodes = env('NODES', '127.0.0.1:5060').split(',');
// Порог — по замеру на чистом прогоне: на линии 32 (единственной в конфиге) score
// 0.78–0.82 — станция окрашивает голос. Чужая речь < 0.4, своя фраза ~0.2,
// 33% потерь — 0.13: порог 0.7 их разделяет с запасом.
export const minScore = Number(env('MIN_SCORE', 0.7));
// Линия — имя в Request-URI (sip:32@…). Станция звучит одной линией — 32 (G.711 с
// «окраской» старой линии), другие имена она приводит к ней же.
export const line = env('LINE', '32');
const recordDir = env('RECORD_DIR', 'recordings');

// Каждый говорит свою фразу: по фразе видно, КОГО именно слышит абонент.
// phrase-c — phrase-a задом наперёд: речеподобный сигнал, с phrase-a не совпадает.
export const phrase = {
  A: sip.audio(open('./refs/phrase-a.wav', 'b')),
  B: sip.audio(open('./refs/phrase-b.wav', 'b')),
  C: sip.audio(open('./refs/phrase-c.wav', 'b')),
};

sip.options({ record: 'onFailure', recordDir });

// Сколько слушать после соединения, с. Фразы крутятся по кругу с момента, когда
// абонент снял трубку, а не с момента соединения: пришедший позже попадает в
// середину чужой фразы. До конца текущего повтора — до 3 с, потом целый повтор —
// ещё 3 с. При 4 с (как в примере xk6-sip, где обе стороны начинают вместе)
// целого повтора могло не набраться: score 0.24 вместо 0.99 на исправном мосту.
export const listen = 7;

// Адрес станции для каждого устройства: к Device (объект хоста) поля не добавить.
const station = {};

// device('A', 0) — абонент a@kontakt.test, звонит на узел nodes[0] (по кругу).
export function device(name, node) {
  const addr = nodes[node % nodes.length];
  const d = new sip.Device({
    device: name,
    registrar: `sip:${addr}`,
    register: false,
    user: `${name.toLowerCase()}@kontakt.test`,
    audio: phrase[name],
    record: true,
  });
  station[name] = `sip:${line}@${addr}`;
  return d;
}

// step записывает проверку и останавливает сценарий на первом провале.
export function step(name, value) {
  if (!check(value, { [name]: (v) => v !== false && v !== null && v !== undefined })) {
    fail(name);
  }
  return value;
}

// pickUp — «снять трубку»: INVITE на станцию. 200 OK приходит сразу, ещё до
// собеседника; пока его нет, станция играет гудки или шум эфира.
export function pickUp(d) {
  const c = step(`${d.id} снимает трубку (${station[d.id]})`, d.call({ callee: station[d.id] }));
  step(`${d.id}: станция ответила 200 OK`, c.expectConnected('5s'));
  step(`${d.id} слышит станцию`, c.isHeard('3s'));
  return c;
}

// keep сохраняет WAV ноги, если проверка не прошла.
function keep(leg, name, ok) {
  if (!ok) {
    leg.saveRecording(`${recordDir}/${leg.callId.replace(/[^\w.-]/g, '_')}.wav`);
  }
  return step(name, ok);
}

// hears — нога leg слышит фразу whose чисто: без провалов и обрезанного начала.
export function hears(who, leg, whose) {
  const q = step(`${who}: звук сравнён с фразой ${whose}`, leg.compareAudio(phrase[whose]));
  console.log(`${who} vs ${whose}: ${JSON.stringify(q)}`);
  keep(leg, `${who} слышит ${whose} (score ${q.score} >= ${minScore})`, q.score >= minScore);
  keep(leg, `${who}: без провалов (${q.gaps} мс)`, q.gaps < 100);
  return q;
}

// notHears — фразы whose в том, что слышал who, нет: нет эха и перепутанных мостов.
export function notHears(who, leg, whose) {
  const q = step(`${who}: звук сравнён с фразой ${whose}`, leg.compareAudio(phrase[whose]));
  keep(leg, `${who} не слышит ${whose} (score ${q.score} < 0.5)`, q.score < 0.5);
  return q;
}

// results — шаги в порядке выполнения, затем пороги: { name, ok, detail }.
function results(data) {
  const out = [];
  const walk = (g) => {
    for (const c of g.checks || []) {
      out.push({ name: c.name, ok: c.fails === 0, detail: `${c.fails} of ${c.passes + c.fails} failed` });
    }
    (g.groups || []).forEach(walk);
  };
  walk(data.root_group);
  for (const [metric, m] of Object.entries(data.metrics)) {
    for (const [expr, t] of Object.entries(m.thresholds || {})) {
      out.push({ name: `threshold ${metric}: ${expr}`, ok: t.ok, detail: 'threshold crossed' });
    }
  }
  return out;
}

const xml = (s) => String(s).replace(/[<>&"]/g, (c) => ({ '<': '&lt;', '>': '&gt;', '&': '&amp;', '"': '&quot;' })[c]);

function junit(suite, rs) {
  const failures = rs.filter((r) => !r.ok).length;
  const cases = rs.map((r) => {
    const tc = `    <testcase name="${xml(r.name)}" classname="${xml(suite)}"`;
    return r.ok ? `${tc}/>` : `${tc}>\n      <failure message="${xml(r.name)}">${xml(r.detail)}</failure>\n    </testcase>`;
  });
  return `<?xml version="1.0" encoding="UTF-8"?>
<testsuites tests="${rs.length}" failures="${failures}">
  <testsuite name="${xml(suite)}" tests="${rs.length}" failures="${failures}">
${cases.join('\n')}
  </testsuite>
</testsuites>
`;
}

// Сводка в консоль — свои шаги, без k6-summary с jslib.k6.io: сценарий не должен
// зависеть от доступности внешнего сайта.
export function handleSummary(data) {
  const junitPath = env('JUNIT', 'junit.xml');
  const suite = env('SUITE', junitPath.replace(/^.*[\\/]/, '').replace(/\.xml$/, ''));
  return {
    stdout: results(data).map((r) => `  ${r.ok ? '✓' : '✗'} ${r.name}`).join('\n') + '\n',
    [junitPath]: junit(suite, results(data)),
  };
}

// Положить всё, что оставил проваленный сценарий: иначе станция соединит
// «призрака» со следующим сценарием.
export function teardown() {
  sip.shutdown();
}
