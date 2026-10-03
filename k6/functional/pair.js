// Пара: A и B снимают трубку, станция соединяет их, каждый слышит другого и не
// слышит себя (с NODES из нескольких входов — на разных входах). A кладёт трубку — B остаётся на линии: звонок не рвётся, станция
// возвращает его в очередь.
import { sleep } from 'k6';
import { device, step, pickUp, hears, notHears, listen } from './lib.js';
export { options, handleSummary, teardown } from './lib.js';

const A = device('A', 0);
const B = device('B', 1);

export default function () {
  const a = pickUp(A);
  const b = pickUp(B);
  sleep(listen);

  hears('A', a, 'B');
  hears('B', b, 'A');
  notHears('A', a, 'A');
  notHears('B', b, 'B');

  a.hangup();
  step('A: трубка положена', a.expectDisconnected('5s'));
  step('A: положил сам', a.howCompleted().endedBy === 'local');
  sleep(2);
  step(`B: звонок жив после ухода A (${b.state()})`, b.state() === 'connected');

  b.hangup();
  step('B: трубка положена', b.expectDisconnected('5s'));
}
