// Рулетка: собеседник ушёл — следующий. A и B говорят, C ждёт. A кладёт
// трубку, и станция перекоммутирует B к C ВНУТРИ ТОГО ЖЕ звонка B: без нового
// INVITE и без BYE. Это главное обещание станции.
import { sleep } from 'k6';
import { device, step, pickUp, hears, notHears, listen } from './lib.js';
export { options, handleSummary, teardown } from './lib.js';

const A = device('A', 0);
const B = device('B', 1);
const C = device('C', 0);

export default function () {
  const a = pickUp(A);
  const b = pickUp(B);
  sleep(listen);
  hears('B', b, 'A');

  const c = pickUp(C); // пара занята — C в очереди, слышит гудки
  sleep(1);
  notHears('C', c, 'A');
  notHears('C', c, 'B');

  const bCall = b.callId;
  a.hangup();
  step('A: трубка положена', a.expectDisconnected('5s'));
  sleep(listen);

  step('B: тот же звонок, без нового INVITE', b.callId === bCall && b.state() === 'connected');
  hears('C', c, 'B');
  hears('B', b, 'C');
  notHears('C', c, 'A');

  b.hangup();
  c.hangup();
  step('B: трубка положена', b.expectDisconnected('5s'));
  step('C: трубка положена', c.expectDisconnected('5s'));
}
