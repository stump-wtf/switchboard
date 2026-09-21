package metrics

// Label limiter tests
//
// Pins the SPEC-0023 REQ-5 mechanism: the first N distinct values keep their own label for the
// life of the process, every later value reports as "__other__", admission never flaps, and the
// bound holds exactly under concurrent first sightings.
//
// @joestump-agent 09/21/2026 - Added for issue #273 (SPEC-0023 story 1).

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestLabelLimiterAdmitsCapThenOverflows(t *testing.T) {
	l := newLabelLimiter(50)
	for i := range 50 {
		q := fmt.Sprintf("queue-%02d", i)
		if got := l.label(q); got != q {
			t.Fatalf("value %d of 50: got %q, want it admitted as itself", i+1, got)
		}
	}
	for _, q := range []string{"queue-50", "queue-51", "late-arrival"} {
		if got := l.label(q); got != Other {
			t.Errorf("%q past the cap: got %q, want %q", q, got, Other)
		}
	}
	// Sticky: admitted values keep their own label after the cap is reached, however often asked.
	for i := range 50 {
		q := fmt.Sprintf("queue-%02d", i)
		if got := l.label(q); got != q {
			t.Errorf("admitted %q after overflow: got %q, want its own label", q, got)
		}
	}
	// And an overflowed value never gets promoted later.
	if got := l.label("queue-50"); got != Other {
		t.Errorf("overflowed value re-asked: got %q, want %q", got, Other)
	}
}

func TestLabelLimiterDefaultCap(t *testing.T) {
	for _, cap := range []int{0, -1} {
		l := newLabelLimiter(cap)
		if l.limit != DefaultLabelCap {
			t.Errorf("newLabelLimiter(%d).limit = %d, want %d", cap, l.limit, DefaultLabelCap)
		}
	}
}

func TestLabelLimiterRejectsUnsafeValuesWithoutSpendingASlot(t *testing.T) {
	l := newLabelLimiter(1)
	for _, v := range []string{
		"",              // empty is indistinguishable from an absent label
		Other,           // the sentinel must not be claimable by a queue named after it
		"bad-\xff-utf8", // the client library panics on invalid UTF-8
		strings.Repeat("q", maxLimitedValueBytes+1), // absurdly long
	} {
		if got := l.label(v); got != Other {
			t.Errorf("label(%q) = %q, want %q", v, got, Other)
		}
	}
	// None of the above consumed the single slot.
	if got := l.label("reviews"); got != "reviews" {
		t.Fatalf("first real value after rejected ones: got %q, want it admitted", got)
	}
	// A value exactly at the length bound is still admissible (into a fresh limiter).
	edge := strings.Repeat("q", maxLimitedValueBytes)
	if got := newLabelLimiter(1).label(edge); got != edge {
		t.Errorf("value at the %d-byte bound was not admitted", maxLimitedValueBytes)
	}
}

// TestLabelLimiterConcurrentAdmission races many goroutines on first sightings of overlapping
// values. The admitted set must end at exactly the cap, and every value must get one consistent
// answer across all goroutines — never its own label on one call and __other__ on another. Run
// with -race.
func TestLabelLimiterConcurrentAdmission(t *testing.T) {
	const (
		limit      = 50
		values     = 200
		goroutines = 32
	)
	l := newLabelLimiter(limit)
	answers := make([][]string, goroutines)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			got := make([]string, values)
			// Each goroutine walks the values from a different offset so first sightings collide.
			for i := range values {
				v := (i + g*7) % values
				got[v] = l.label(fmt.Sprintf("q%03d", v))
			}
			answers[g] = got
		}(g)
	}
	wg.Wait()

	admitted := 0
	for v := range values {
		want := answers[0][v]
		for g := 1; g < goroutines; g++ {
			if answers[g][v] != want {
				t.Fatalf("q%03d: goroutine 0 saw %q, goroutine %d saw %q — admission flapped", v, want, g, answers[g][v])
			}
		}
		if want != Other {
			admitted++
		}
	}
	if admitted != limit {
		t.Fatalf("admitted %d distinct values, want exactly %d", admitted, limit)
	}
	if n := len(l.admitted); n != limit {
		t.Fatalf("admitted set holds %d values, want %d", n, limit)
	}
}
