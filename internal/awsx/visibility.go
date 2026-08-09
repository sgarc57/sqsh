package awsx

import (
	"context"
	"time"
)

// KeepVisible extends a message's visibility while a command runs (§8.5): it
// calls ChangeVisibility(visibility) every visibility/3 until ctx is done,
// and stops extending once total elapsed time would exceed limit, so a wedged
// process cannot hold a message indefinitely. onExtend, if set, is called
// after every extension attempt with its error.
//
// It blocks; run it in its own goroutine and cancel ctx when the command ends.
func KeepVisible(ctx context.Context, q VisibilityChanger, receiptHandle string,
	visibility, limit time.Duration, onExtend func(error)) {
	interval := visibility / 3
	if interval <= 0 {
		return
	}
	start := time.Now()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// The next extension makes the message invisible until now+visibility;
		// never push that past start+limit.
		remaining := limit - time.Since(start)
		if remaining <= 0 {
			return
		}
		ext := min(visibility, remaining.Truncate(time.Second))
		if ext <= 0 {
			return
		}
		err := q.ChangeVisibility(ctx, receiptHandle, ext)
		if onExtend != nil && ctx.Err() == nil {
			onExtend(err)
		}
	}
}
