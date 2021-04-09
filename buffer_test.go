package batcher_test

import (
	"sync"
	"testing"
	"time"

	gobatcher "github.com/plasne/go-batcher"
	"github.com/stretchr/testify/assert"
)

func TestBuffer_New(t *testing.T) {
	buffer := gobatcher.NewBuffer(10)
	assert.Equal(t, uint32(0), buffer.Size())
	assert.Equal(t, uint32(10), buffer.Max())
}

func TestBuffer_Enqueue(t *testing.T) {
	buffer := gobatcher.NewBuffer(1)
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.IOperation) {})
	op := gobatcher.NewOperation(watcher, 0, struct{}{}, false)
	ref := op.(*gobatcher.Operation)
	err := buffer.Enqueue(ref, false)
	assert.Nil(t, err, "expecting no error on enqueue")
}

func TestBuffer_ErrorOnFull(t *testing.T) {
	var err error
	buffer := gobatcher.NewBuffer(1)
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.IOperation) {})
	op1 := gobatcher.NewOperation(watcher, 0, struct{}{}, false).(*gobatcher.Operation)
	err = buffer.Enqueue(op1, true)
	assert.Nil(t, err, "expecting no error on enqueue")
	op2 := gobatcher.NewOperation(watcher, 0, struct{}{}, false).(*gobatcher.Operation)
	err = buffer.Enqueue(op2, true)
	assert.Equal(t, gobatcher.BufferFullError{}, err, "expecting the buffer is full because it has a size of 1")
}

func TestBuffer_BlockOnFull(t *testing.T) {
	buffer := gobatcher.NewBuffer(1)
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.IOperation) {})
	done := make(chan struct{})
	wg := &sync.WaitGroup{}
	wg.Add(2)
	go func() {
		wg.Wait()
		close(done)
	}()
	for i := 0; i < 2; i++ {
		go func() {
			op := gobatcher.NewOperation(watcher, 0, struct{}{}, false).(*gobatcher.Operation)
			err := buffer.Enqueue(op, false)
			assert.Nil(t, err, "expecting no error on enqueue")
			wg.Done()
		}()
	}
	select {
	case <-done:
		assert.Fail(t, "expecting a timeout because the enqueue is blocking")
	case <-time.After(10 * time.Millisecond):
		// expect this timeout
	}
}

func TestBuffer_SizeIsCorrect(t *testing.T) {
	var err error
	buffer := gobatcher.NewBuffer(10)
	assert.Equal(t, uint32(0), buffer.Size())
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.IOperation) {})

	op1 := gobatcher.NewOperation(watcher, 0, struct{}{}, false).(*gobatcher.Operation)
	err = buffer.Enqueue(op1, false)
	assert.Nil(t, err, "expecting no error on enqueue")
	assert.Equal(t, uint32(1), buffer.Size())

	op2 := gobatcher.NewOperation(watcher, 0, struct{}{}, false).(*gobatcher.Operation)
	err = buffer.Enqueue(op2, false)
	assert.Nil(t, err, "expecting no error on enqueue")
	assert.Equal(t, uint32(2), buffer.Size())

	buffer.Top()

	buffer.Remove()
	assert.Nil(t, err, "expecting no error on remove")
	assert.Equal(t, uint32(1), buffer.Size())

	buffer.Remove()
	assert.Nil(t, err, "expecting no error on remove")
	assert.Equal(t, uint32(0), buffer.Size())
}

func TestBuffer_Skip(t *testing.T) {
	var err error
	buffer := gobatcher.NewBuffer(10)
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.IOperation) {})

	op1 := gobatcher.NewOperation(watcher, 0, struct{}{}, false).(*gobatcher.Operation)
	err = buffer.Enqueue(op1, false)
	assert.Nil(t, err, "expecting no error on enqueue")

	op2 := gobatcher.NewOperation(watcher, 0, struct{}{}, false).(*gobatcher.Operation)
	err = buffer.Enqueue(op2, false)
	assert.Nil(t, err, "expecting no error on enqueue")

	assert.Equal(t, op1, buffer.Top())
	assert.Equal(t, op2, buffer.Skip())
	assert.Nil(t, buffer.Skip())
	assert.Equal(t, uint32(2), buffer.Size())
}

func TestBuffer_Remove(t *testing.T) {
	var err error
	buffer := gobatcher.NewBuffer(10)
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.IOperation) {})

	op1 := gobatcher.NewOperation(watcher, 0, struct{}{}, false).(*gobatcher.Operation)
	err = buffer.Enqueue(op1, false)
	assert.Nil(t, err, "expecting no error on enqueue")

	op2 := gobatcher.NewOperation(watcher, 0, struct{}{}, false).(*gobatcher.Operation)
	err = buffer.Enqueue(op2, false)
	assert.Nil(t, err, "expecting no error on enqueue")

	assert.Equal(t, op1, buffer.Top())
	assert.Equal(t, uint32(2), buffer.Size())
	assert.Equal(t, op2, buffer.Remove())
	assert.Equal(t, uint32(1), buffer.Size())
	assert.Nil(t, buffer.Remove())
	assert.Equal(t, uint32(0), buffer.Size())
}

func TestBuffer_TopIsEmpty(t *testing.T) {
	buffer := gobatcher.NewBuffer(10)
	assert.Nil(t, buffer.Top(), "expecting no head")
}

func TestBuffer_Clear(t *testing.T) {
	var err error
	buffer := gobatcher.NewBuffer(10)
	watcher := gobatcher.NewWatcher(func(batch []gobatcher.IOperation) {})
	op := gobatcher.NewOperation(watcher, 0, struct{}{}, false).(*gobatcher.Operation)
	err = buffer.Enqueue(op, false)
	assert.Nil(t, err, "expecting no error on enqueue")
	buffer.Clear()
	assert.Equal(t, uint32(0), buffer.Size())
	assert.Nil(t, buffer.Top())
}

// TODO document flush-start, flush-done