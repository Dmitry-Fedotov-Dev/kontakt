// Ушёл из очереди: A снимает трубку, не дождавшись собеседника кладёт её. Потом
// приходят B и C — станция соединяет их друг с другом, а не с ушедшим A.
import { sleep } from 'k6';
import { device, step, pickUp, hears, listen } from './lib.js';
export { options, handleSummary, teardown } from './lib.js';

const A = device('A', 0);
const B = device('B', 1);
const C = device('C', 0);

export default function () {
  const a = pickUp(A);
  sleep(1);
  a.hangup();
  step('A: трубка положена из очереди', a.expectDisconnected('5s'));
  step('A: положил сам', a.howCompleted().endedBy === 'local');

  const b = pickUp(B);
  const c = pickUp(C);
  sleep(listen);
  hears('B', b, 'C');
  hears('C', c, 'B');

  b.hangup();
  step('B: трубка положена', b.expectDisconnected('5s'));
  sleep(2);
  step(`C: звонок жив после ухода B (${c.state()})`, c.state() === 'connected');
  c.hangup();
  step('C: трубка положена', c.expectDisconnected('5s'));
}
