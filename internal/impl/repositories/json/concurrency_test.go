package json

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChatRepository_ConcurrentWritesToDifferentChats mirrors the kind of
// concurrency test aiagent's own concurrency_test.go implies: two
// goroutines writing many different chats in parallel must never corrupt
// either file. Each write goes through NewChatRepository's atomic
// temp-file-then-rename path (Design §2/§6), so a reader should only ever
// observe a fully-written, validly-encoded chat — never a torn/partial one.
func TestChatRepository_ConcurrentWritesToDifferentChats(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	const perGoroutine = 50
	var wg sync.WaitGroup
	wg.Add(2)

	errCh := make(chan error, 2*perGoroutine)

	writer := func(prefix string) {
		defer wg.Done()
		for i := 0; i < perGoroutine; i++ {
			id := fmt.Sprintf("%s-%d", prefix, i)
			chat := sampleChat(id)
			chat.SessionState = []byte(fmt.Sprintf("session-%s-%d", prefix, i))
			if err := repo.Create(ctx, chat); err != nil {
				errCh <- fmt.Errorf("create %s: %w", id, err)
			}
		}
	}

	go writer("left")
	go writer("right")
	wg.Wait()
	close(errCh)

	for err := range errCh {
		assert.NoError(t, err)
	}

	// Every chat from both goroutines must be readable, unmangled, and
	// carry exactly the SessionState bytes it was written with — proof
	// that concurrent writes to distinct files never interleaved.
	for _, prefix := range []string{"left", "right"} {
		for i := 0; i < perGoroutine; i++ {
			id := fmt.Sprintf("%s-%d", prefix, i)
			got, err := repo.Get(ctx, id)
			require.NoError(t, err, "reading %s", id)
			assert.Equal(t, id, got.ID)
			assert.Equal(t, fmt.Sprintf("session-%s-%d", prefix, i), string(got.SessionState))
		}
	}

	chats, err := repo.List(ctx)
	require.NoError(t, err)
	assert.Len(t, chats, 2*perGoroutine)
}

// TestChatRepository_ConcurrentUpdatesToSameChat exercises the same-ID case
// called out in the write doc comment: concurrent updates to one chat must
// never corrupt the file on disk, even though which write "wins" is
// unspecified.
func TestChatRepository_ConcurrentUpdatesToSameChat(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	require.NoError(t, repo.Create(ctx, sampleChat("shared")))

	const updates = 50
	var wg sync.WaitGroup
	wg.Add(updates)
	for i := 0; i < updates; i++ {
		go func(i int) {
			defer wg.Done()
			chat := sampleChat("shared")
			chat.SessionState = []byte(fmt.Sprintf("state-%d", i))
			_ = repo.Update(ctx, chat)
		}(i)
	}
	wg.Wait()

	// The file must still be valid JSON, fully written, and decodable —
	// never a torn mix of two concurrent writes.
	got, err := repo.Get(ctx, "shared")
	require.NoError(t, err)
	assert.Equal(t, "shared", got.ID)
	assert.Contains(t, string(got.SessionState), "state-")
}
