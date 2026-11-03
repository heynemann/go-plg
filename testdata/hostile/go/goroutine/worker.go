package goroutine

import "context"

func work(ctx context.Context) {
	results := make(chan int, 1)

	go compute(ctx, results)
	<-results
}

func compute(_ context.Context, results chan<- int) {
	results <- 42
}
