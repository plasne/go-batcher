package batcher_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	gobatcher "github.com/plasne/go-batcher/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
)

// NOTE: mock.AssertExpectations was not used because it iterates all private properties of the mocked object and sometimes
// this is not threadsafe due to mutex locks or atomic. https://github.com/stretchr/testify/issues/625

func TestBatcher_Enqueue_IsAllowedBeforeStartup(t *testing.T) {
	batcher := gobatcher.NewBatcher()
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {})
	operation := gobatcher.NewOperation(watcher, 0, struct{}{}, false)
	err := batcher.Enqueue(operation)
	assert.NoError(t, err, "expect enqueue to be fine even if not started")
}

func TestBatcher_Enqueue_MustIncludeAnOperation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batcher := gobatcher.NewBatcher()
	err := batcher.Start(ctx)
	assert.NoError(t, err, "expecting no errors on startup")
	err = batcher.Enqueue(nil)
	if err != nil {
		_ = err.Error() // improves code coverage
	}
	assert.Equal(t, gobatcher.NoOperationError, err, "expect a no-operation error")
}

func TestBatcher_Enqueue_OperationsRequiresAWatcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batcher := gobatcher.NewBatcher()
	err := batcher.Start(ctx)
	assert.NoError(t, err, "expecting no errors on startup")
	operation := gobatcher.NewOperation(nil, 0, struct{}{}, false)
	err = batcher.Enqueue(operation)
	if err != nil {
		_ = err.Error() // improves code coverage
	}
	assert.Equal(t, gobatcher.NoWatcherError, err, "expect a no-watcher error")
}

func TestBatcher_Enqueue_OperationsCannotExceedMaxCapacity_Reserved(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := gobatcher.NewSharedResource().
		WithReservedCapacity(1000)
	batcher := gobatcher.NewBatcher().
		WithRateLimiter(res)
	err := batcher.Start(ctx)
	assert.NoError(t, err, "expecting no errors on startup")
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {})
	operation := gobatcher.NewOperation(watcher, 2000, struct{}{}, false)
	err = batcher.Enqueue(operation)
	if err != nil {
		_ = err.Error() // improves code coverage
	}
	assert.Equal(t, gobatcher.TooExpensiveError, err, "expect a too-expensive-error error")
}

func TestBatcher_Enqueue_OperationsCannotExceedMaxCapacity_SharedAndReserved(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := &mockLeaseManager{}
	mgr.On("RaiseEventsTo", mock.Anything)
	res := gobatcher.NewSharedResource().
		WithReservedCapacity(2000).
		WithSharedCapacity(10000, mgr).
		WithFactor(1000)
	batcher := gobatcher.NewBatcher().
		WithRateLimiter(res)
	err := batcher.Start(ctx)
	assert.NoError(t, err, "expecting no errors on startup")
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {})
	good := gobatcher.NewOperation(watcher, 11000, struct{}{}, false)
	err = batcher.Enqueue(good)
	assert.NoError(t, err)
	bad := gobatcher.NewOperation(watcher, 13000, struct{}{}, false)
	err = batcher.Enqueue(bad)
	if err != nil {
		_ = err.Error() // improves code coverage
	}
	assert.Equal(t, gobatcher.TooExpensiveError, err, "expect a too-expensive-error error")
	mgr.AssertNumberOfCalls(t, "RaiseEventsTo", 1)
}

func TestBatcher_Enqueue_OperationsCannotBeAttemptedMoreThanXTimes(t *testing.T) {
	// NOTE: this test works by recursively enqueuing the same operation over and over again until it fails
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batcher := gobatcher.NewBatcher().
		WithFlushInterval(1 * time.Millisecond)
	err := batcher.Start(ctx)
	assert.NoError(t, err, "expecting no errors on startup")
	var mu sync.Mutex
	attempts := 0
	func() {
		var op gobatcher.Operation
		enqueue := func() {
			mu.Lock()
			defer mu.Unlock()
			attempts++
			if eerr := batcher.Enqueue(op); eerr != nil {
				if eerr != nil {
					_ = eerr.Error() // improves code coverage
				}
				assert.Equal(t, gobatcher.TooManyAttemptsError, eerr, "expect the error to be too-many-attempts")
				return
			}
			if attempts > 3 {
				assert.FailNow(t, "the max-attempts governor didn't work, we have tried too many times")
			}
		}
		watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
			enqueue()
		}).WithMaxAttempts(3)
		op = gobatcher.NewOperation(watcher, 100, struct{}{}, false)
		enqueue()
		time.Sleep(100 * time.Millisecond)
	}()
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 4, attempts, "expect enqueue will be accepted 3 times, but fail on the 4th")
}

func TestBatcher_Enqueue_OperationsCannotBeEnqueuedMultipleTimesAtOnce(t *testing.T) {
	multipleEnqueueTests := []bool{false, true}
	for _, batching := range multipleEnqueueTests {
		testName := fmt.Sprintf("batch:%v", batching)
		t.Run(testName, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			batcher := gobatcher.NewBatcher().
				WithFlushInterval(1 * time.Millisecond)
			var updateCountersMutex sync.Mutex
			var attempts uint32
			func() {
				count := 0
				watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
					updateCountersMutex.Lock()
					defer updateCountersMutex.Unlock()
					for _, entry := range batch {
						count++
						if entry.Attempt() > attempts {
							attempts = entry.Attempt()
						}
					}
					if count > 3 {
						return
					}
				}).WithMaxAttempts(1)
				// NOTE: enqueue before start to ensure nothing is processed when enqueueing
				var err error
				var op = gobatcher.NewOperation(watcher, 100, struct{}{}, batching)
				err = batcher.Enqueue(op)
				assert.NoError(t, err, "expecting no error on enqueue")
				err = batcher.Enqueue(op)
				assert.NoError(t, err, "expecting no error on enqueue")
				err = batcher.Enqueue(op)
				assert.NoError(t, err, "expecting no error on enqueue")
				err = batcher.Enqueue(op)
				assert.NoError(t, err, "expecting no error on enqueue")
				err = batcher.Start(ctx)
				assert.NoError(t, err, "expecting no errors on startup")
				time.Sleep(100 * time.Millisecond)
			}()
			updateCountersMutex.Lock()
			defer updateCountersMutex.Unlock()
			assert.Equal(t, uint32(4), attempts, "expecting 4 attempts were made (even though max is 1) because enqueue happened before processing")
		})
	}
}

func TestBatcher_Enqueue_WillBlockCallerIfBafferFull_Default(t *testing.T) {
	batcher := gobatcher.NewBatcherWithBuffer(1)
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {})
	var err error
	op1 := gobatcher.NewOperation(watcher, 0, struct{}{}, false)
	err = batcher.Enqueue(op1)
	assert.NoError(t, err, "expecting no error on enqueue")
	done := make(chan bool, 1)
	go func() {
		op2 := gobatcher.NewOperation(watcher, 0, struct{}{}, false)
		err = batcher.Enqueue(op2)
		assert.NoError(t, err, "expecting no error on enqueue")
		done <- true
	}()
	timeout := false
	select {
	case <-done:
		assert.Fail(t, "did not expect the enqueue to complete because there was no buffer")
	case <-time.After(500 * time.Millisecond):
		timeout = true
	}
	assert.True(t, timeout, "expecting the second enqueue to timeout (was blocking)")
}

func TestBatcher_Enqueue_WillThrowErrorIfBufferIsFull_Config(t *testing.T) {
	batcher := gobatcher.NewBatcherWithBuffer(1).
		WithErrorOnFullBuffer()
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {})
	var err error
	op1 := gobatcher.NewOperation(watcher, 0, struct{}{}, false)
	err = batcher.Enqueue(op1)
	assert.NoError(t, err, "expecting no error on enqueue")
	op2 := gobatcher.NewOperation(watcher, 0, struct{}{}, false)
	err = batcher.Enqueue(op2)
	if err != nil {
		_ = err.Error() // improves code coverage
	}
	assert.Equal(t, gobatcher.BufferFullError, err, "expecting the buffer to be full")
}

func TestBatcher_Enqueue_AddingOperationsIncreasesNumInBuffer(t *testing.T) {
	batcher := gobatcher.NewBatcher()
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {})
	op := gobatcher.NewOperation(watcher, 100, struct{}{}, false)
	err := batcher.Enqueue(op)
	assert.NoError(t, err, "expecting no error on enqueue")
	cap := batcher.OperationsInBuffer()
	assert.Equal(t, uint32(1), cap, "expecting the number of operations to match the number enqueued")
}

func TestBatcher_Enqueue_MarkingDoneReducesNumInBuffer(t *testing.T) {
	multipleDoneTests := []bool{false, true}
	for _, batching := range multipleDoneTests {
		testName := fmt.Sprintf("batch:%v", batching)
		t.Run(testName, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			batcher := gobatcher.NewBatcher()
			wg := sync.WaitGroup{}
			wg.Add(4)
			watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
				for i := 0; i < len(batch); i++ {
					wg.Done()
				}
			})
			for i := 0; i < 4; i++ {
				op := gobatcher.NewOperation(watcher, 100, struct{}{}, batching)
				err := batcher.Enqueue(op)
				assert.NoError(t, err, "expecting no error on enqueue")
			}
			before := batcher.OperationsInBuffer()
			err := batcher.Start(ctx)
			assert.NoError(t, err, "expecting no error on enqueue")
			wg.Wait()
			after := batcher.OperationsInBuffer()
			assert.Equal(t, uint32(4), before, "expecting the buffer to include all records before processing")
			assert.Equal(t, uint32(0), after, "expecting the buffer to be empty after processing")
		})
	}
}

func TestBatcher_NeedsCapacity_CostUpdatesTheTarget(t *testing.T) {
	batcher := gobatcher.NewBatcher()
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {})
	op := gobatcher.NewOperation(watcher, 100, struct{}{}, false)
	err := batcher.Enqueue(op)
	assert.NoError(t, err, "expecting no error on enqueue")
	cap := batcher.NeedsCapacity()
	assert.Equal(t, uint32(100), cap, "expecting the capacity to match the operation cost")
}

func TestBatcher_NeedsCapacity_MarkingDoneReducesTarget(t *testing.T) {
	multipleDoneTests := []bool{false, true}
	for _, batching := range multipleDoneTests {
		testName := fmt.Sprintf("batch:%v", batching)
		t.Run(testName, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			batcher := gobatcher.NewBatcher()
			wg := sync.WaitGroup{}
			wg.Add(4)
			watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
				for i := 0; i < len(batch); i++ {
					wg.Done()
				}
			})
			for i := 0; i < 4; i++ {
				op := gobatcher.NewOperation(watcher, 100, struct{}{}, batching)
				err := batcher.Enqueue(op)
				assert.NoError(t, err, "expecting no error on enqueue")
			}
			before := batcher.NeedsCapacity()
			err := batcher.Start(ctx)
			assert.NoError(t, err, "expecting no error on enqueue")
			wg.Wait()
			time.Sleep(100 * time.Millisecond)
			after := batcher.NeedsCapacity()
			assert.Equal(t, uint32(400), before, "expecting the cost to be the sum of all operations")
			assert.Equal(t, uint32(0), after, "expecting the cost be 0 after processing")
		})
	}
}

func TestBatcher_NeedsCapacity_EnsureOperationCostsResultInRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := gobatcher.NewSharedResource().
		WithReservedCapacity(10000)
	batcher := gobatcher.NewBatcher().
		WithRateLimiter(res).
		WithFlushInterval(1 * time.Millisecond).
		WithEmitRequest()
	var mu sync.Mutex
	var max int
	batcher.AddListener(func(event string, val int, msg string, metadata interface{}) {
		switch event {
		case gobatcher.RequestEvent:
			mu.Lock()
			defer mu.Unlock()
			if val > max {
				max = val
			}
		}
	})
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		time.Sleep(400 * time.Millisecond)
	})
	var err error
	op1 := gobatcher.NewOperation(watcher, 800, struct{}{}, false)
	err = batcher.Enqueue(op1)
	assert.NoError(t, err, "expecting no error on enqueue")
	op2 := gobatcher.NewOperation(watcher, 300, struct{}{}, false)
	err = batcher.Enqueue(op2)
	assert.NoError(t, err, "expecting no error on enqueue")
	err = batcher.Start(ctx)
	assert.NoError(t, err, "expecting no error on start")
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1100, max, "expecting the request to be the sum of the operations")
}

func TestBatcher_NeedsCapacity_EnsureOperationCostsResultInTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := &mockLeaseManager{}
	mgr.On("RaiseEventsTo", mock.Anything)
	res := gobatcher.NewSharedResource().
		WithSharedCapacity(10000, mgr).
		WithFactor(1000)
	batcher := gobatcher.NewBatcher().
		WithRateLimiter(res).
		WithFlushInterval(1 * time.Millisecond)
	var mu sync.Mutex
	var max int
	res.AddListener(func(event string, val int, msg string, metadata interface{}) {
		switch event {
		case gobatcher.TargetEvent:
			mu.Lock()
			defer mu.Unlock()
			if val > max {
				max = val
			}
		}
	})
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		time.Sleep(400 * time.Millisecond)
	})
	var err error
	op1 := gobatcher.NewOperation(watcher, 800, struct{}{}, false)
	err = batcher.Enqueue(op1)
	assert.NoError(t, err, "expecting no error on enqueue")
	op2 := gobatcher.NewOperation(watcher, 300, struct{}{}, false)
	err = batcher.Enqueue(op2)
	assert.NoError(t, err, "expecting no error on enqueue")
	err = batcher.Start(ctx)
	assert.NoError(t, err, "expecting no error on start")
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1100, max, "expecting the request to be the sum of the operations")
	mgr.AssertNumberOfCalls(t, "RaiseEventsTo", 1)
}

func TestBatcher_Pause_LastsForExpectedDuration(t *testing.T) {
	testCases := map[string]struct {
		input  time.Duration
		output int64
	}{
		"500 ms (default)": {input: time.Duration(0), output: 500},
		"750 ms":           {input: 750 * time.Millisecond, output: 750},
	}
	for testName, testCase := range testCases {
		t.Run(testName, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			batcher := gobatcher.NewBatcher().
				WithPauseTime(testCase.input)
			err := batcher.Start(ctx)
			assert.NoError(t, err, "not expecting a start error")
			wg := sync.WaitGroup{}
			wg.Add(2)
			var paused, resumed time.Time
			batcher.AddListener(func(event string, val int, msg string, metadata interface{}) {
				switch event {
				case gobatcher.PauseEvent:
					paused = time.Now()
					wg.Done()
				case gobatcher.ResumeEvent:
					resumed = time.Now()
					wg.Done()
				}
			})
			batcher.Pause()
			done := make(chan struct{})
			go func() {
				defer close(done)
				wg.Wait()
			}()
			select {
			case <-done:
				// saw a pause and resume
			case <-time.After(1 * time.Second):
				assert.Fail(t, "expected to be resumed before now")
			}
			len := resumed.Sub(paused)
			assert.GreaterOrEqual(t, len.Milliseconds(), testCase.output, "expecting the pause to be at least %v ms", testCase.output)
		})
	}
}

func TestBatcher_Pause_EnsureMultiplePausesDoNotIncreaseTheTime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batcher := gobatcher.NewBatcher()
	err := batcher.Start(ctx)
	assert.NoError(t, err, "not expecting a start error")
	wg := sync.WaitGroup{}
	wg.Add(2)
	var paused, resumed time.Time
	batcher.AddListener(func(event string, val int, msg string, metadata interface{}) {
		switch event {
		case gobatcher.PauseEvent:
			paused = time.Now()
			wg.Done()
		case gobatcher.ResumeEvent:
			resumed = time.Now()
			wg.Done()
		}
	})
	batcher.Pause()
	time.Sleep(100 * time.Millisecond)
	batcher.Pause()
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()
	select {
	case <-done:
		// saw a pause and resume
	case <-time.After(1 * time.Second):
		assert.Fail(t, "expected to be resumed before now")
	}
	len := resumed.Sub(paused)
	assert.GreaterOrEqual(t, len.Milliseconds(), int64(500), "expecting the pause to be at least 500 ms")
	assert.Less(t, len.Milliseconds(), int64(600), "expecting the pause to be under 600 ms")
}

func TestBatcher_Pause_EnsureNegativeDurationUses500ms_Default(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batcher := gobatcher.NewBatcher().
		WithPauseTime(-100 * time.Millisecond)
	err := batcher.Start(ctx)
	assert.NoError(t, err, "not expecting a start error")
	wg := sync.WaitGroup{}
	wg.Add(2)
	var paused, resumed time.Time
	batcher.AddListener(func(event string, val int, msg string, metadata interface{}) {
		switch event {
		case gobatcher.PauseEvent:
			paused = time.Now()
			wg.Done()
		case gobatcher.ResumeEvent:
			resumed = time.Now()
			wg.Done()
		}
	})
	batcher.Pause()
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()
	select {
	case <-done:
		// saw a pause and resume
	case <-time.After(1 * time.Second):
		assert.Fail(t, "expected to be resumed before now")
	}
	len := resumed.Sub(paused)
	assert.GreaterOrEqual(t, len.Milliseconds(), int64(500), "expecting the pause to be at least 500 ms")
	assert.Less(t, len.Milliseconds(), int64(600), "expecting the pause to be under 600 ms")
}

func TestBatcher_Pause_EnsureNoProcessingHappensDuringAPause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batcher := gobatcher.NewBatcher()
	err := batcher.Start(ctx)
	assert.NoError(t, err, "not expecting a start error")
	wg := sync.WaitGroup{}
	wg.Add(2)
	resumed := false
	batcher.AddListener(func(event string, val int, msg string, metadata interface{}) {
		switch event {
		case gobatcher.ResumeEvent:
			resumed = true
			wg.Done()
		}
	})
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		assert.True(t, resumed, "all batches should be raised after resume")
		wg.Done()
	})
	batcher.Pause()
	op := gobatcher.NewOperation(watcher, 100, struct{}{}, false)
	err = batcher.Enqueue(op)
	assert.NoError(t, err, "not expecting an enqueue error")
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()
	select {
	case <-done:
		// saw a pause and resume
	case <-time.After(1 * time.Second):
		assert.Fail(t, "expected to be completed before now")
	}
	assert.True(t, resumed, "expecting the pause to have resumed")
}

func TestBatcher_Start_IsCallableOnlyOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batcher := gobatcher.NewBatcher()
	var err1, err2 error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		err1 = batcher.Start(ctx)
		wg.Done()
	}()
	go func() {
		err2 = batcher.Start(ctx)
		wg.Done()
	}()
	wg.Wait()
	if err1 != nil {
		assert.Equal(t, gobatcher.ImproperOrderError, err1)
	} else if err2 != nil {
		assert.Equal(t, gobatcher.ImproperOrderError, err2)
	} else {
		t.Errorf("expected one of the two calls to fail (err1: %v) (err2: %v)", err1, err2)
	}
}

func TestBatcher_Start_EnsureThatMixedOperationsAreBatchedOrNotAsAppropriate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batcher := gobatcher.NewBatcher()
	var err error
	wg := sync.WaitGroup{}
	wg.Add(2)
	var op1, op2, op3 gobatcher.Operation
	var count uint32 = 0
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		atomic.AddUint32(&count, uint32(len(batch)))
		switch len(batch) {
		case 1:
			assert.Equal(t, op2, batch[0], "expect that the batch has op2")
		case 2:
			assert.Equal(t, op1, batch[0], "expect that the batch has op1 and op3")
			assert.Equal(t, op3, batch[1], "expect that the batch has op1 and op3")
		}
		wg.Done()
	})
	op1 = gobatcher.NewOperation(watcher, 100, struct{}{}, true)
	err = batcher.Enqueue(op1)
	assert.NoError(t, err, "not expecting an enqueue error")
	op2 = gobatcher.NewOperation(watcher, 100, struct{}{}, false)
	err = batcher.Enqueue(op2)
	assert.NoError(t, err, "not expecting an enqueue error")
	op3 = gobatcher.NewOperation(watcher, 100, struct{}{}, true)
	err = batcher.Enqueue(op3)
	assert.NoError(t, err, "not expecting an enqueue error")
	err = batcher.Start(ctx)
	assert.NoError(t, err, "not expecting a startup error")
	wg.Wait()
	assert.Equal(t, uint32(3), count, "expect 3 operations to be completed")
}

func TestBatcher_Start_EnsureFullBatchesAreFlushed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batcher := gobatcher.NewBatcher().
		WithFlushInterval(1 * time.Millisecond)
	var count uint32 = 0
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		atomic.AddUint32(&count, 1)
		assert.Equal(t, 3, len(batch), "expect batches to have 3 operations each")
	}).WithMaxBatchSize(3)
	for i := 0; i < 9; i++ {
		op := gobatcher.NewOperation(watcher, 100, struct{}{}, true)
		err := batcher.Enqueue(op)
		assert.NoError(t, err, "not expecting an enqueue error")
	}
	err := batcher.Start(ctx)
	assert.NoError(t, err, "not expecting a startup error")
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, uint32(3), atomic.LoadUint32(&count), "expect 3 batches")
}

func TestBatcher_Start_InitializationAfterStartCausesPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := gobatcher.NewSharedResource().
		WithReservedCapacity(100)
	batcher := gobatcher.NewBatcher()
	err := batcher.Start(ctx)
	assert.NoError(t, err, "not expecting a start error")
	assert.PanicsWithError(t, gobatcher.InitializationOnlyError.Error(), func() { batcher.WithRateLimiter(res) })
	assert.PanicsWithError(t, gobatcher.InitializationOnlyError.Error(), func() { batcher.WithFlushInterval(1 * time.Millisecond) })
	assert.PanicsWithError(t, gobatcher.InitializationOnlyError.Error(), func() { batcher.WithCapacityInterval(1000) })
	assert.PanicsWithError(t, gobatcher.InitializationOnlyError.Error(), func() { batcher.WithAuditInterval(1 * time.Millisecond) })
	assert.PanicsWithError(t, gobatcher.InitializationOnlyError.Error(), func() { batcher.WithMaxOperationTime(10 * time.Millisecond) })
	assert.PanicsWithError(t, gobatcher.InitializationOnlyError.Error(), func() { batcher.WithPauseTime(1 * time.Millisecond) })
	assert.PanicsWithError(t, gobatcher.InitializationOnlyError.Error(), func() { batcher.WithErrorOnFullBuffer() })
	assert.PanicsWithError(t, gobatcher.InitializationOnlyError.Error(), func() { batcher.WithEmitBatch() })
}

func TestBatcher_Loop_Shutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	batcher := gobatcher.NewBatcher()
	done := make(chan bool)
	batcher.AddListener(func(event string, val int, msg string, metadata interface{}) {
		switch event {
		case gobatcher.ShutdownEvent:
			close(done)
		}
	})
	err := batcher.Start(ctx)
	assert.NoError(t, err, "not expecting a start error")
	cancel()
	select {
	case <-done:
		// success
	case <-time.After(1 * time.Second):
		// timeout
		assert.Fail(t, "expected shutdown but didn't see one even after 1 second")
	}
}

func TestBatcher_Loop_EnsureOperationsAreFlushedInExpectedTimes(t *testing.T) {
	testCases := map[string]struct {
		interval time.Duration
		enqueue  int
		wait     time.Duration
		expect   uint32
	}{
		"-200ms (default to 100)": {interval: -200 * time.Millisecond, enqueue: 4, wait: 250 * time.Millisecond, expect: 2},
		"100ms (default)":         {interval: 0 * time.Millisecond, enqueue: 4, wait: 250 * time.Millisecond, expect: 2},
		"300ms":                   {interval: 300 * time.Millisecond, enqueue: 4, wait: 650 * time.Millisecond, expect: 2},
	}
	for testName, testCase := range testCases {
		t.Run(testName, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			res := gobatcher.NewSharedResource().
				WithReservedCapacity(100)
			batcher := gobatcher.NewBatcher().
				WithRateLimiter(res).
				WithFlushInterval(testCase.interval)
			var count uint32 = 0
			watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
				atomic.AddUint32(&count, uint32(len(batch)))
			})
			for i := 0; i < testCase.enqueue; i++ {
				op := gobatcher.NewOperation(watcher, 100, struct{}{}, false)
				err := batcher.Enqueue(op)
				assert.NoError(t, err, "not expecting an enqueue error")
			}
			err := batcher.Start(ctx)
			assert.NoError(t, err, "not expecting a start error")
			time.Sleep(testCase.wait)
			assert.Equal(t, testCase.expect, atomic.LoadUint32(&count), "expecting %v operations to be completed given the %v interval and capacity for only a single operation", testCase.interval, testCase.expect)
		})
	}
}

func TestBatcher_Loop_EnsureCapacityRequestsAreRaisedInExpectedTimes(t *testing.T) {
	testCases := map[string]struct {
		interval time.Duration
		wait     time.Duration
		expect   uint32
	}{
		"-200ms (default to 100ms)": {interval: 0 * time.Millisecond, wait: 250 * time.Millisecond, expect: 2},
		"100ms (default)":           {interval: 0 * time.Millisecond, wait: 250 * time.Millisecond, expect: 2},
		"300ms":                     {interval: 300 * time.Millisecond, wait: 650 * time.Millisecond, expect: 2},
	}
	for testName, testCase := range testCases {
		t.Run(testName, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mgr := &mockLeaseManager{}
			mgr.On("RaiseEventsTo", mock.Anything)
			res := gobatcher.NewSharedResource().
				WithSharedCapacity(10000, mgr).
				WithFactor(1000)
			batcher := gobatcher.NewBatcher().
				WithRateLimiter(res).
				WithCapacityInterval(testCase.interval).
				WithEmitRequest()
			var count uint32 = 0
			batcher.AddListener(func(event string, val int, msg string, metadata interface{}) {
				switch event {
				case gobatcher.RequestEvent:
					atomic.AddUint32(&count, 1)
				}
			})
			watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {})
			op := gobatcher.NewOperation(watcher, 800, struct{}{}, false)
			err := batcher.Enqueue(op)
			assert.NoError(t, err, "not expecting an enqueue error")
			err = batcher.Start(ctx)
			assert.NoError(t, err, "not expecting a start error")
			time.Sleep(testCase.wait)
			assert.Equal(t, testCase.expect, atomic.LoadUint32(&count), "expecting %v capacity requests given the %v interval and capacity for only a single operation", testCase.interval, testCase.expect)
			mgr.AssertNumberOfCalls(t, "RaiseEventsTo", 1)
		})
	}
}

func TestBatcher_Loop_EnsureLongRunningOperationsAreStillMarkedDone_Watcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batcher := gobatcher.NewBatcher().
		WithFlushInterval(1 * time.Millisecond)
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		// NOTE: simulate a long-running operation
		time.Sleep(200 * time.Millisecond)
	}).WithMaxOperationTime(50 * time.Millisecond)
	op := gobatcher.NewOperation(watcher, 100, struct{}{}, false)
	err := batcher.Enqueue(op)
	assert.NoError(t, err, "not expecting an enqueue error")
	before := batcher.NeedsCapacity()
	err = batcher.Start(ctx)
	assert.NoError(t, err, "not expecting a start error")
	time.Sleep(100 * time.Millisecond)
	after := batcher.NeedsCapacity()
	assert.Equal(t, uint32(100), before, "expecting 100 capacity request before starting")
	assert.Equal(t, uint32(0), after, "expecting 0 capacity request after max-operation-time")
}

func TestBatcher_Loop_EnsureLongRunningOperationsAreStillMarkedDone_Batcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batcher := gobatcher.NewBatcher().
		WithFlushInterval(1 * time.Millisecond).
		WithMaxOperationTime(50 * time.Millisecond)
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		// NOTE: simulate a long-running operation
		time.Sleep(200 * time.Millisecond)
	})
	op := gobatcher.NewOperation(watcher, 100, struct{}{}, false)
	err := batcher.Enqueue(op)
	assert.NoError(t, err, "not expecting an enqueue error")
	before := batcher.NeedsCapacity()
	err = batcher.Start(ctx)
	assert.NoError(t, err, "not expecting a start error")
	time.Sleep(100 * time.Millisecond)
	after := batcher.NeedsCapacity()
	assert.Equal(t, uint32(100), before, "expecting 100 capacity request before starting")
	assert.Equal(t, uint32(0), after, "expecting 0 capacity request after max-operation-time")
}

func TestBatcher_Loop_EnsureLongRunningOperationsAreNotMarkedDoneBefore1mDefault(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batcher := gobatcher.NewBatcher().
		WithFlushInterval(1 * time.Millisecond)
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		// NOTE: simulate a long-running operation
		time.Sleep(400 * time.Millisecond)
	})
	op := gobatcher.NewOperation(watcher, 100, struct{}{}, false)
	err := batcher.Enqueue(op)
	assert.NoError(t, err, "not expecting an enqueue error")
	before := batcher.NeedsCapacity()
	err = batcher.Start(ctx)
	assert.NoError(t, err, "not expecting a start error")
	time.Sleep(200 * time.Millisecond)
	after := batcher.NeedsCapacity()
	assert.Equal(t, uint32(100), before, "expecting 100 capacity request before starting")
	assert.Equal(t, uint32(100), after, "expecting 100 capacity request after 200 milliseconds")
}

func TestBatcher_Audit_DemonstrateAnAuditPass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batcher := gobatcher.NewBatcher().
		WithFlushInterval(1 * time.Millisecond).
		WithAuditInterval(1 * time.Millisecond).
		WithMaxOperationTime(1 * time.Millisecond)
	var passed, failed uint32
	batcher.AddListener(func(event string, val int, msg string, metadata interface{}) {
		switch event {
		case gobatcher.AuditPassEvent:
			atomic.AddUint32(&passed, 1)
		case gobatcher.AuditFailEvent:
			atomic.AddUint32(&failed, 1)
		}
	})
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {})
	op := gobatcher.NewOperation(watcher, 100, struct{}{}, false)
	err := batcher.Enqueue(op)
	assert.NoError(t, err, "not expecting an enqueue error")
	err = batcher.Start(ctx)
	assert.NoError(t, err, "not expecting a start error")
	time.Sleep(100 * time.Millisecond)
	assert.Greater(t, atomic.LoadUint32(&passed), uint32(0), "expecting audit-pass because done() was called before max-operation-time (1m default)")
	assert.Equal(t, uint32(0), atomic.LoadUint32(&failed), "expecting no audit-fail messages")
}

func TestBatcher_Audit_DemonstrateAnAuditFail_Target(t *testing.T) {
	// NOTE: this sets a batcher max-op-time to 1ms and a watcher max-op-time to 1m allowing for the batch to be around longer than it thinks it should be
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batcher := gobatcher.NewBatcher().
		WithFlushInterval(1 * time.Millisecond).
		WithAuditInterval(1 * time.Millisecond).
		WithMaxOperationTime(1 * time.Millisecond)
	var failed uint32
	batcher.AddListener(func(event string, val int, msg string, metadata interface{}) {
		if event == gobatcher.AuditFailEvent && msg == gobatcher.AuditMsgFailureOnTarget {
			atomic.AddUint32(&failed, 1)
		}
	})
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		time.Sleep(20 * time.Millisecond)
	}).WithMaxOperationTime(1 * time.Minute)
	op := gobatcher.NewOperation(watcher, 100, struct{}{}, false)
	err := batcher.Enqueue(op)
	assert.NoError(t, err, "not expecting an enqueue error")
	err = batcher.Start(ctx)
	assert.NoError(t, err, "not expecting a start error")
	time.Sleep(100 * time.Millisecond)
	assert.Greater(t, atomic.LoadUint32(&failed), uint32(0), "expecting an audit failure because done() was not called and max-operation-time was exceeded")
	assert.Equal(t, uint32(0), batcher.NeedsCapacity())
}

func TestBatcher_Audit_DemonstrateAnAuditFail_InFlight(t *testing.T) {
	// NOTE: this sets a batcher max-op-time to 1ms and a watcher max-op-time to 1m allowing for the batch to be around longer than it thinks it should be
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batcher := gobatcher.NewBatcher().
		WithFlushInterval(1 * time.Millisecond).
		WithAuditInterval(1 * time.Millisecond).
		WithMaxOperationTime(1 * time.Millisecond).
		WithMaxConcurrentBatches(1) // ensures there can be inflight errors
	var failed uint32
	batcher.AddListener(func(event string, val int, msg string, metadata interface{}) {
		if event == gobatcher.AuditFailEvent && msg == gobatcher.AuditMsgFailureOnInflight {
			atomic.AddUint32(&failed, 1)
		}
	})
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		time.Sleep(20 * time.Millisecond)
	}).WithMaxOperationTime(1 * time.Minute)
	op := gobatcher.NewOperation(watcher, 0, struct{}{}, false)
	err := batcher.Enqueue(op)
	assert.NoError(t, err, "not expecting an enqueue error")
	err = batcher.Start(ctx)
	assert.NoError(t, err, "not expecting a start error")
	time.Sleep(100 * time.Millisecond)
	assert.Greater(t, atomic.LoadUint32(&failed), uint32(0), "expecting an audit failure because done() was not called and max-operation-time was exceeded")
	assert.Equal(t, uint32(0), batcher.Inflight())
}

func TestBatcher_Audit_DemonstrateAnAuditFail_TargetAndInFlight(t *testing.T) {
	// NOTE: this sets a batcher max-op-time to 1ms and a watcher max-op-time to 1m allowing for the batch to be around longer than it thinks it should be
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batcher := gobatcher.NewBatcher().
		WithFlushInterval(1 * time.Millisecond).
		WithAuditInterval(1 * time.Millisecond).
		WithMaxOperationTime(1 * time.Millisecond).
		WithMaxConcurrentBatches(1) // ensures there can be inflight errors
	var failed uint32
	batcher.AddListener(func(event string, val int, msg string, metadata interface{}) {
		if event == gobatcher.AuditFailEvent && msg == gobatcher.AuditMsgFailureOnTargetAndInflight {
			atomic.AddUint32(&failed, 1)
		}
	})
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		time.Sleep(20 * time.Millisecond)
	}).WithMaxOperationTime(1 * time.Minute)
	op := gobatcher.NewOperation(watcher, 100, struct{}{}, false)
	err := batcher.Enqueue(op)
	assert.NoError(t, err, "not expecting an enqueue error")
	err = batcher.Start(ctx)
	assert.NoError(t, err, "not expecting a start error")
	time.Sleep(100 * time.Millisecond)
	assert.Greater(t, atomic.LoadUint32(&failed), uint32(0), "expecting an audit failure because done() was not called and max-operation-time was exceeded")
	assert.Equal(t, uint32(0), batcher.NeedsCapacity())
	assert.Equal(t, uint32(0), batcher.Inflight())
}

func TestBatcher_Audit_DemonstrateAnAuditSkip(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batcher := gobatcher.NewBatcher().
		WithFlushInterval(1 * time.Millisecond).
		WithAuditInterval(1 * time.Millisecond)
	var skipped uint32
	batcher.AddListener(func(event string, val int, msg string, metadata interface{}) {
		switch event {
		case gobatcher.AuditSkipEvent:
			atomic.AddUint32(&skipped, 1)
		}
	})
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		time.Sleep(20 * time.Millisecond)
	})
	var err error
	op := gobatcher.NewOperation(watcher, 100, struct{}{}, false)
	err = batcher.Enqueue(op)
	assert.NoError(t, err, "not expecting an enqueue error")
	err = batcher.Start(ctx)
	assert.NoError(t, err, "not expecting a start error")
	time.Sleep(100 * time.Millisecond)
	assert.Greater(t, atomic.LoadUint32(&skipped), uint32(0), "expect that something in the buffer but max-operation-time is still valid, will cause skips")
}

func TestBatcher_Flush(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var err error
	batcher := gobatcher.NewBatcher().WithFlushInterval(10 * time.Minute)
	err = batcher.Start(ctx)
	assert.NoError(t, err, "not expecting a start error")
	completed := make(chan bool, 1)
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		completed <- true
	})
	op := gobatcher.NewOperation(watcher, 100, struct{}{}, false)
	err = batcher.Enqueue(op)
	assert.NoError(t, err, "not expecting an enqueue error")
	batcher.Flush()
	select {
	case <-completed:
	case <-time.After(1 * time.Second):
		assert.Fail(t, "expected the manual flush to have completed the batch before the timeout")
	}
}

type TestMaxConcurrentBatchesSuite struct {
	suite.Suite
	batcher  gobatcher.Batcher
	listener uuid.UUID
	wg       *sync.WaitGroup
	cancel   context.CancelFunc
}

func (s *TestMaxConcurrentBatchesSuite) BeforeTest(suiteName, testName string) {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg = &sync.WaitGroup{}
	s.batcher = gobatcher.NewBatcher().
		WithFlushInterval(10 * time.Minute).
		WithEmitBatch().
		WithEmitFlush()
	switch testName {
	case "TestBatcher_BatchPacking":
		s.batcher.WithMaxConcurrentBatches(1)
	default:
		s.batcher.WithMaxConcurrentBatches(2)
	}
	s.listener = s.batcher.AddListener(func(event string, val int, msg string, metadata interface{}) {
		switch event {
		case gobatcher.FlushDoneEvent:
			s.wg.Done()
		case gobatcher.BatchEvent:
			s.wg.Add(1)
		}
	})
	err := s.batcher.Start(ctx)
	s.NoError(err, "not expecting a start error")
}

func (s *TestMaxConcurrentBatchesSuite) TearDownTest() {
	s.batcher.RemoveListener(s.listener)
	s.cancel()
}

func (s *TestMaxConcurrentBatchesSuite) TestBatcher_ConcurrencyIsEnforced() {
	var batches uint32
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		atomic.AddUint32(&batches, 1)
		time.Sleep(15 * time.Millisecond) // NOTE: simulate a long-running operation
		s.wg.Done()
	})
	for i := 0; i < 3; i++ {
		op := gobatcher.NewOperation(watcher, 0, struct{}{}, false)
		err := s.batcher.Enqueue(op)
		s.NoError(err, "not expecting an enqueue error")
	}
	s.wg.Add(1)
	s.batcher.Flush()
	s.wg.Wait()
	s.Equal(uint32(2), atomic.LoadUint32(&batches))
	s.Equal(uint32(1), s.batcher.OperationsInBuffer())
}

func (s *TestMaxConcurrentBatchesSuite) TestBatcher_ConcurrencyIsEnforcedWithBatchable() {
	var batches uint32
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		atomic.AddUint32(&batches, 1)
		time.Sleep(15 * time.Millisecond) // NOTE: simulate a long-running operation
		s.wg.Done()
	}).WithMaxBatchSize(2)
	for i := 0; i < 5; i++ {
		op := gobatcher.NewOperation(watcher, 0, struct{}{}, true)
		err := s.batcher.Enqueue(op)
		s.NoError(err, "not expecting an enqueue error")
	}
	s.wg.Add(1)
	s.batcher.Flush()
	s.wg.Wait()
	s.Equal(uint32(2), atomic.LoadUint32(&batches))
	s.Equal(uint32(1), s.batcher.OperationsInBuffer())
}

func (s *TestMaxConcurrentBatchesSuite) TestBatcher_SlotsAreAvailableOnDone() {
	var batches uint32
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		atomic.AddUint32(&batches, 1)
		time.Sleep(15 * time.Millisecond) // NOTE: simulate a long-running operation
		s.wg.Done()
	})
	for i := 0; i < 5; i++ {
		op := gobatcher.NewOperation(watcher, 0, struct{}{}, false)
		err := s.batcher.Enqueue(op)
		s.NoError(err, "not expecting an enqueue error")
	}
	s.wg.Add(1)
	s.batcher.Flush()
	s.wg.Wait()
	s.Equal(uint32(2), atomic.LoadUint32(&batches))
	s.Equal(uint32(3), s.batcher.OperationsInBuffer())
	s.wg.Add(1)
	s.batcher.Flush()
	s.wg.Wait()
	s.Equal(uint32(4), atomic.LoadUint32(&batches))
	s.Equal(uint32(1), s.batcher.OperationsInBuffer())
}

func (s *TestMaxConcurrentBatchesSuite) TestBatcher_RunningOpHoldsSlot() {
	var batches uint32
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		atomic.AddUint32(&batches, 1)
		s.wg.Done()
		time.Sleep(15 * time.Millisecond) // NOTE: simulate a long-running operation
	})
	for i := 0; i < 3; i++ {
		op := gobatcher.NewOperation(watcher, 0, struct{}{}, false)
		err := s.batcher.Enqueue(op)
		s.NoError(err, "not expecting an enqueue error")
	}
	s.wg.Add(1)
	s.batcher.Flush()
	s.wg.Wait()
	s.wg.Add(1)
	s.batcher.Flush()
	s.wg.Wait()
	s.Equal(uint32(2), atomic.LoadUint32(&batches))
	s.Equal(uint32(1), s.batcher.OperationsInBuffer())
}

func (s *TestMaxConcurrentBatchesSuite) TestBatcher_BatchPacking() {
	var err error
	var batches uint32
	watcher1 := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		s.Equal(2, len(batch))
		atomic.AddUint32(&batches, 1)
		s.wg.Done()
	})
	watcher2 := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
		s.FailNow("expecting never to see this batch raised")
	})
	op1 := gobatcher.NewOperation(watcher1, 0, struct{}{}, true)
	err = s.batcher.Enqueue(op1)
	s.NoError(err, "not expecting an enqueue error")
	op2 := gobatcher.NewOperation(watcher2, 0, struct{}{}, true)
	err = s.batcher.Enqueue(op2)
	s.NoError(err, "not expecting an enqueue error")
	op3 := gobatcher.NewOperation(watcher1, 0, struct{}{}, true)
	err = s.batcher.Enqueue(op3)
	s.NoError(err, "not expecting an enqueue error")
	s.wg.Add(1)
	s.batcher.Flush()
	s.wg.Wait()
	s.Equal(uint32(1), atomic.LoadUint32(&batches), "expecting watcher1 to see a batch once with 2 items")
	s.Equal(uint32(1), s.batcher.OperationsInBuffer())
}

func TestBatcher_MaxConcurrentBatches(t *testing.T) {
	suite.Run(t, new(TestMaxConcurrentBatchesSuite))
}

func TestBatcher_Operation_PayloadIsValid(t *testing.T) {
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {})
	payload := struct{}{}
	operation := gobatcher.NewOperation(watcher, 0, payload, false)
	assert.Equal(t, payload, operation.Payload())
}
