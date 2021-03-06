# Configuration

- [Batcher Configuration](#batcher-configuration)
- [Operation Configuration](#operation-configuration)
- [Watcher Configuration](#watcher-configuration)
- [SharedResource Configuration](#SharedResource-configuration)

## Batcher Configuration

Creating a new Batcher with all defaults looks like this...

```go
batcher := NewBatcher()
```

Creating with all available configuration items might look like this...

```go
batcher := gobatcher.NewBatcherWithBuffer(buffer).
    WithRateLimiter(rateLimiter).
    WithFlushInterval(100 * time.Millisecond).
    WithCapacityInterval(100 * time.Millisecond).
    WithAuditInterval(10 * time.Second).
    WithMaxOperationTime(1 * time.Minute).
    WithPauseTime(500 * time.Millisecond).
    WithErrorOnFullBuffer().
    WithEmitBatch()
```

- __buffer__ [DEFAULT: 10,0000]: The buffer determines how many Operations can be enqueued at a time. When ErrorOnFullBuffer is "false" (the default), the Enqueue() method blocks until a slot is available. When ErrorOnFullBuffer is "true" an error of type `BufferFullError` is returned from Enqueue().

- __WithRateLimiter__ [OPTIONAL]: If provided, it will be used to ensure that the cost of Operations does not exceed the capacity available per second.

- __WithFlushInterval__ [DEFAULT: 100ms]: This determines how often Operations in the buffer are examined. Each time the interval fires, Operations will be dequeued and added to batches or released individually (if not batchable) until such time as the aggregate cost of everything considered in the interval exceeds the capacity allotted this timeslice. For the 100ms default, there will be 10 intervals per second, so the capacity allocated is 1/10th the available capacity. Generally you want FlushInterval to be under 1 second though it could technically go higher.

- __WithCapacityInterval__ [DEFAULT: 100ms]: This determines how often the Batcher asks the rate limiter for capacity. Generally you should leave this alone, and the implementation of what the rate limiter does when Batcher asks it for capacity could be different. For example, when using an SharedResource rate limiter, you could increase it to slow down the number of storage Operations required for sharing capacity. Please be aware that this only applies to Batcher asking for capacity, it doesn't mean the rate limiter will allocate capacity any faster, just that it is being asked more often.

- __WithAuditInterval__ [DEFAULT: 10s]: This determines how often the Target is audited to ensure it is accurate. The Target is manipulated with atomic Operations and abandoned batches are cleaned up after MaxOperationTime so Target should always be accurate. Therefore, we should expect to only see "audit-pass" and "audit-skip" events. This audit interval is a failsafe that if the buffer is empty and the MaxOperationTime (on Batcher only; Watchers are ignored) is exceeded and the Target is greater than zero, it is reset and an "audit-fail" event is raised. Since Batcher is a long-lived process, this audit helps ensure a broken process does not monopolize SharedCapacity when it isn't needed.

- __WithMaxOperationTime__ [DEFAULT: 1m]: This determines how long the system should wait for the Watcher's callback function to be completed before it assumes it is done and decreases the Target anyway. It is critical that the Target reflect the current cost of outstanding Operations. The MaxOperationTime ensures that a batch isn't orphaned and continues reserving capacity long after it is no longer needed. Please note there is also a MaxOperationTime on the Watcher which takes precedent over this time.

- __WithPauseTime__ [DEFAULT: 500ms]: This determines how long the FlushInterval, CapacityInterval, and AuditIntervals are paused when Batcher.Pause() is called. Typically you would pause because the datastore cannot keep up with the volume of requests (if it happens maybe adjust your rate limiter).

- __WithMaxConcurrentBatches__ [OPTIONAL]: If you specify this option, Batcher will ensure that the number of Inflight batches does not exceed this value. Batches are still only produced on the FlushInterval. When a batch is marked done, the concurrency slot is freed for another batch. If you do not specify this option, there is no limit to the number of batches that can be raised at a time (each running in a separate goroutine).

- __WithErrorOnFullBuffer__ [OPTIONAL]: Normally the Enqueue() method will block if the buffer is full, however, you can set this configuration flag if you want it to return an error instead.

- __WithEmitFlush__ [OPTIONAL]: There may be certain cases (for example, unit testing) when it is helpful to know when a flush starts (event: "flush-start") and when it is complete (event: "flush-done"). If you have a use-case for this, you can emit those events. This is off by default as this will generate a massive number of events.

- __WithEmitBatch__ [OPTIONAL]: DO NOT USE IN PRODUCTION. For unit testing it may be useful to batches that are raised across all Watchers. Setting this flag causes a "batch" event to be emitted with the operations in a batch set as the metadata (see the sample). You would not want this in production because it will diminish performance but it will also allow anyone with access to the batcher to see operations raised whether they have access to the Watcher or not.

After creation, you must call Start() on a Batcher to begin processing. You can enqueue Operations before starting if desired (though keep in mind that there is a Buffer size and you will fill it if the Batcher is not running).

## Operation Configuration

Creating a new Operation with all defaults might look like this...

```go
operation := gobatcher.NewOperation(&watcher, cost, payload, allowBatch)
```

- __watcher__ [REQUIRED]: To create a new Operation, you must pass a reference to a Watcher. When this Operation is put into a batch, it is to this Watcher that it will be raised.

- __cost__ [REQUIRED]: When you create a new Operation, you must provide a cost of type `uint32`. You can supply "0" but this Operation will only be effectively rate limited if it has a non-zero cost.

- __payload__ [REQUIRED]: When you create a new Operation, you will provide a payload of type `interface{}`. This could be the entity you intend to write to the datastore, it could be a query that you intend to run, it could be a wrapper object containing a payload and metadata, or anything else that might be helpful so that you know what to process.

- __allowBatch__ [REQUIRED]: Set to TRUE if the Operation is eligible to be batched with other Operations. Otherwise, it will be raised as a batch of a single Operation.

## Watcher Configuration

Creating a new Watcher with all defaults might look like this...

```go
watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
    // your processing function goes here
})
```

Creating with all available configuration options might look like this...

```go
watcher := gobatcher.NewWatcher(func(batch []gobatcher.Operation) {
    // your processing function goes here
}).
    WithMaxAttempts(3).
    WithMaxBatchSize(500).
    WithMaxOperationTime(1 * time.Minute)
```

- __processing_func__ [REQUIRED]: To create a new Watcher, you must provide a callback function that accepts a batch of Operations. The provided function will be called as each batch is available for processing. When the callback function is completed, it will reduce the Target by the cost of all Operations in the batch. If for some reason the processing is "stuck" in this function, they Target will be reduced after MaxOperationTime. Every time this function is called with a batch it is run as a new goroutine so anything inside could cause race conditions with the rest of your code - use atomic, sync, etc. as appropriate.

- __WithMaxAttempts__ [OPTIONAL]: If there are transient errors, you can enqueue the same Operation again. If you do not provide MaxAttempts, it will allow you to enqueue as many times as you like. Instead, if you specify MaxAttempts, the Enqueue() method will return `TooManyAttemptsError` if you attempt to enqueue it too many times. You could check this yourself instead of just enqueuing, but this provides a simple pattern of always attempt to enqueue then handle errors.

- __WithMaxBatchSize__ [OPTIONAL]: This determines the maximum number of Operations that will be raised in a single batch. This does not guarantee that batches will be of this size (constraints such rate limiting might reduce the size), but it does guarantee they will not be larger.

- __WithMaxOperationTime__ [OPTIONAL]: This determines how long the system should wait for the callback function to be completed on the batch before it assumes it is done and decreases the Target anyway. It is critical that the Target reflect the current cost of outstanding Operations. The MaxOperationTime ensures that a batch isn't orphaned and continues reserving capacity long after it is no longer needed. If MaxOperationTime is not provided on the Watcher, the Batcher MaxOperationTime is used.

## SharedResource configuration

Creating a new SharedResource might look like this...

```go
resource := gobatcher.NewSharedResource().
    WithReservedCapacity(100)
```

Creating with all available configuration options might look like this...

```go
resource := gobatcher.NewSharedResource().
    WithReservedCapacity(2000).
    WithSharedCapacity(2000, leaseManager).
    WithFactor(1000).
    WithMaxInterval(1)
```

- __WithReservedCapacity__ [OPTIONAL]: You could run SharedResource with only SharedCapacity, but then every time it needs to run a single operation, the latency of that operation would be increased by the time it takes to allocate a partition. To improve the latency of these one-off operations, you may reserve some capacity so it is always available. Generally, you would reserve a small capacity and share the bulk of the capacity.

- __WithSharedCapacity__ [OPTIONAL]: To create a provisioned resource, you must provide the capacity that will be shared across all processes. Based on this and Factor, the correct number of partitions can be created in the Azure Storage Account. Shared capacity will require a leaseManager that is responsible for provisioning partitions and managing exclusive leases for those partitions.

- __WithFactor__ [DEFAULT: 1]: The SharedCapacity will be divided by the Factor (rounded up) to determine the number of partitions to create when Provision() is called. For example, if you have 10,200 of SharedCapacity and a Factor of 1000, then there will be 11 partitions. Whenever a partition is obtained by SharedResource, it will be worth a single Factor or 1000 RU. For predictability, the SharedCapacity should always be evenly divisible by Factor. SharedResource does not support more than 500 partitions.

- __WithMaxInterval__ [DEFAULT: 500ms]: This determines the maximum time that the SharedResource will wait before attempting to allocate a new partition (if one is needed). The interval is random to improve entropy, but it won't be longer than this specified time. If you want fewer storage transactions, you could increase this time, but it would slow down how quickly the SharedResource can obtain new RUs.

After creation, you must call Provision() and then Start() on any rate limiters to begin processing.

### AzureBlobLeaseManager

Creating an AzureBlobLeaseManager might look like this...

```go
leaseManager := gobatcher.NewAzureBlobLeaseManager(accountName, containerName, masterKey)
```

__accountName__ [REQUIRED]: The account name of the Azure Storage Account that will host the zero-byte blobs that serve as partitions for capacity.

__containerName__ [REQUIRED]: The container name that will host the zero-byte blobs that serve as partitions for capacity.

__masterKey__ [REQUIRED]: There needs to be some way to authenticate access to the Azure Storage Account, right now only master keys are supported.

After creation, you will provide the leaseManager as a parameter to SharedResource.WithSharedCapacity().
