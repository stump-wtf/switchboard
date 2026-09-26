package store

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// BenchmarkClaimNextContention measures claim throughput under contention: eight workers draining
// one endpoint's queue with claim_next. SPEC-0034 REQ-18 bounds the attempt-history write cost at
// 10% of the pre-change baseline; run it on both sides of a change with
// `go test -run '^$' -bench ClaimNextContention -count 5 ./internal/store/`.
func BenchmarkClaimNextContention(b *testing.B) {
	s, ctx := testStore(b)
	ep := seedEndpoint(b, s, ctx, "claim-bench")
	for i := 0; i < b.N; i++ {
		if _, _, err := s.CreateTodo(ctx, CreateTodoParams{EndpointID: ep, Queue: "bench", Title: "b",
			IdempotencyKey: fmt.Sprintf("bench-%d", i)}); err != nil {
			b.Fatalf("seed: %v", err)
		}
	}
	const workers = 8
	jobs := make(chan struct{}, b.N)
	for i := 0; i < b.N; i++ {
		jobs <- struct{}{}
	}
	close(jobs)
	b.ResetTimer()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for range jobs {
				if _, err := s.ClaimNext(ctx, ep, []string{"bench"}, fmt.Sprintf("w%d", w), time.Minute); err != nil {
					b.Errorf("claim_next: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
}
