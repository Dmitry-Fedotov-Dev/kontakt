package signal

import (
	"testing"
	"time"
)

// Три жетона, после трёх «положить» — ноль; первый возвращается через refill после первой траты,
// дальше по одному, не больше запаса.
func TestTokens(t *testing.T) {
	var b tokenBank
	const refill = 80 * time.Millisecond
	for i := 0; i < 3; i++ {
		if !b.Spend("a", 3, refill) {
			t.Fatalf("жетон %d не опустился", i+1)
		}
	}
	if b.Spend("a", 3, refill) {
		t.Fatal("опустился четвёртый жетон")
	}
	if n, wait := b.State("a", 3, refill); n != 0 || wait <= 0 || wait > refill {
		t.Fatalf("пусто: %d жетонов, ждать %v", n, wait)
	}
	if n, _ := b.State("b", 3, refill); n != 3 {
		t.Fatalf("у нового %d жетонов", n)
	}
	time.Sleep(refill + 10*time.Millisecond)
	if n, _ := b.State("a", 3, refill); n != 1 {
		t.Fatalf("через refill %d жетонов, ждали 1", n)
	}
	time.Sleep(3 * refill)
	if n, wait := b.State("a", 3, refill); n != 3 || wait != 0 {
		t.Fatalf("через долгое время %d жетонов, ждать %v — ждали полный запас", n, wait)
	}
	if len(b.m) != 0 {
		t.Fatal("полный кошелёк не забыт")
	}
}
