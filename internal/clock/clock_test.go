package clock

import (
	"testing"
	"time"
)

func TestSeconds(t *testing.T) {
	f := NewFake(time.Unix(1784277970, 47882000)) // .047882s
	got := Seconds(f)
	want := 1784277970.047882
	if diff := got - want; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("Seconds() = %f, want %f", got, want)
	}
}

func TestFakeAdvanceAndSleep(t *testing.T) {
	start := time.Unix(1000, 0)
	f := NewFake(start)
	if !f.Now().Equal(start) {
		t.Fatalf("Now() = %v, want %v", f.Now(), start)
	}
	f.Advance(5 * time.Second)
	if got := f.Now().Sub(start); got != 5*time.Second {
		t.Errorf("after Advance(5s), elapsed = %v", got)
	}
	f.Sleep(2 * time.Second)
	if got := f.Now().Sub(start); got != 7*time.Second {
		t.Errorf("after Sleep(2s), elapsed = %v", got)
	}
	if len(f.Slept) != 1 || f.Slept[0] != 2*time.Second {
		t.Errorf("Slept = %v, want [2s]", f.Slept)
	}
}

func TestFakeAfterFiresImmediately(t *testing.T) {
	f := NewFake(time.Unix(1000, 0))
	select {
	case ts := <-f.After(3 * time.Second):
		if got := ts.Unix(); got != 1003 {
			t.Errorf("After fired with %d, want 1003", got)
		}
	default:
		t.Fatal("Fake.After did not fire immediately")
	}
}

func TestSystemNowMonotonicish(t *testing.T) {
	var c Clock = System{}
	a := c.Now()
	b := c.Now()
	if b.Before(a) {
		t.Errorf("System clock went backwards: %v then %v", a, b)
	}
}
