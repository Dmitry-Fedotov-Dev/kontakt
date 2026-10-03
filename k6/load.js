// Нагрузка на станцию через xk6-sip: каждый VU — один абонент-софтфон, который
// снова и снова снимает трубку, ждёт собеседника, говорит HOLD секунд и кладёт
// трубку. Пары станция подбирает сама, случайно — как в жизни.
//
//   IP=127.0.0.1 SIP_UDP=:5060 ./scripts/run.sh
//   k6 run -e VUS=40 -e DURATION=2m -e HOLD=8 k6/load.js
//
// С мониторингом (monitoring/): k6 пишет свои метрики в тот же Prometheus, и на
// дашборде видно и станцию, и абонентов:
//   K6_PROMETHEUS_RW_SERVER_URL=http://127.0.0.1:9092/api/v1/write K6_FEATURES=native-histograms \
//   k6 run -o experimental-prometheus-rw --tag testid=run-1 k6/load.js
import sip from 'k6/x/sip';
import { check, sleep } from 'k6';
import { Rate, Trend } from 'k6/metrics';

const env = (k, d) => __ENV[k] || d;
const nodes = env('NODES', '127.0.0.1:5060').split(',');
const hold = Number(env('HOLD', 8));
// Линия и порог — как в functional/lib.js: на линии 32 чистый мост даёт 0.78–0.82.
const line = env('LINE', '32');
const minScore = Number(env('MIN_SCORE', 0.7));
// Звук сравнивается у каждого SAMPLE-го абонента: compareAudio стоит ~15 мс CPU
// и 32 КБ памяти на секунду записи, под нагрузкой — только выборка.
const sample = Number(env('SAMPLE', 4));

export const options = {
  scenarios: {
    subscribers: {
      executor: 'constant-vus',
      vus: Number(env('VUS', 20)),
      duration: env('DURATION', '1m'),
      gracefulStop: `${hold + 10}s`,
    },
  },
  thresholds: {
    sip_call_success: ['rate>0.99'], // станция сняла трубку: 200 OK, а не 486/503/302
    sip_call_setup_time: ['p(95)<300'],
    rtp_audio_heard: ['rate>0.99'], // в трубке хоть что-то: гудки, шум или собеседник
    kontakt_peer_heard: ['rate>0.9'], // в трубке живой собеседник, а не только гудки
    rtp_audio_score: [`p(50)>${Number(__ENV.MIN_SCORE || 0.7)}`], // сравнивается только у выборки, см. SAMPLE
  },
};

// Абонент услышал чью-то фразу (собеседника, а не гудки). Не 100%: пара может
// разойтись посреди фразы, когда собеседник кладёт трубку, — это норма рулетки.
const peerHeard = new Rate('kontakt_peer_heard');
const sessions = new Trend('kontakt_session_duration', true);

// Все говорят одну фразу: собеседник случаен, и с одной фразой на звонок
// приходится одно сравнение, а rtp_audio_score честно отвечает «слышал ли
// человека». Эхо и перепутанные мосты ловят функциональные сценарии, где у
// каждого своя фраза.
const phrase = sip.audio(open('./functional/refs/phrase-a.wav', 'b'));

sip.options({ deviceTag: env('DEVICE_TAG', '') === '1', metricsAddr: env('SIP_METRICS_ADDR', '') });

// __VU в init при разборе options равен 0, отсюда max(). Входы станции — по
// кругу: в кластере с 302-балансировкой xk6-sip 302 не следует, и перекос нагрузки
// был бы отказом, которого у людей нет.
const vu = Math.max(__VU, 1);
const node = nodes[vu % nodes.length];
const recorded = vu % sample === 0;
const ua = new sip.Device({
  device: `sub${vu}`,
  registrar: `sip:${node}`,
  register: false,
  user: `sub${vu}@kontakt.test`,
  audio: phrase,
  record: recorded,
});

export default function () {
  const t0 = Date.now();
  const c = ua.call({ callee: `sip:${line}@${node}` });
  if (!c) return;
  if (!check(c.expectConnected('5s'), { 'станция сняла трубку': (ok) => ok })) {
    c.hangup();
    sleep(1);
    return;
  }
  if (!check(c.isHeard('3s'), { 'в трубке есть звук': (ok) => ok })) {
    console.warn(`${ua.id}: тишина 3 с после 200 OK, ${JSON.stringify(c.mediaStats())}\n${c.trace()}`);
  }
  sleep(hold);

  if (recorded) {
    const q = c.compareAudio(phrase);
    peerHeard.add(q !== null && q.score >= minScore);
  }
  c.hangup();
  c.expectDisconnected('5s');
  sessions.add(Date.now() - t0);
  sleep(Math.random()); // не класть и не снимать трубки всем разом
}

export function teardown() {
  sip.shutdown();
}
