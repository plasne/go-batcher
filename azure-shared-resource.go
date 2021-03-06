package batcher

// NOTE: please review this code which allows for sharing capacity across processes/containers

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type AzureSharedResource struct {
	eventer

	// configuration items that should not change after Provision()
	factor           uint32
	maxInterval      uint32
	sharedCapacity   uint32
	reservedCapacity uint32

	// used for internal operations
	leaseManager leaseManager

	// manage the phase
	phaseMutex sync.Mutex
	phase      int
	shutdown   sync.WaitGroup
	stop       chan bool

	// capacity and target needs to be threadsafe and changes frequently
	capacity uint32
	target   uint32

	// partitions need to be threadsafe and should use the partlock
	partlock   sync.RWMutex
	partitions []*string
}

// This function should be called to create a new AzureSharedResource. The accountName and containerName refer to the details
// of an Azure Storage Account and container that the lease blobs can be created in. If multiple processes are sharing the same
// capacity, they should all point to the same container. The sharedCapacity parameter is the maximum shared capacity for the
// resource. For example, if you provision a Cosmos database with 20k RU, you might set sharedCapacity to 20,000. Capacity is renewed
// every 1 second. Commonly after calling NewAzureSharedResource() you will chain some WithXXXX methods, for instance...
// `NewAzureSharedResource().WithMasterKey(key)`.
func NewAzureSharedResource(accountName, containerName string, sharedCapacity uint32) *AzureSharedResource {
	res := &AzureSharedResource{
		sharedCapacity: sharedCapacity,
	}
	mgr := newAzureBlobLeaseManager(res, accountName, containerName)
	res.leaseManager = mgr
	return res
}

// This allows you to provide mocked objects for container and blob for unit tests.
func (r *AzureSharedResource) WithMocks(container IAzureContainer, blob IAzureBlob) *AzureSharedResource {
	if ablm, ok := r.leaseManager.(*azureBlobLeaseManager); ok {
		ablm.withMocks(container, blob)
	}
	return r
}

// You must provide credentials for the AzureSharedResource to access the Azure Storage Account. Currently, the only supported method
// is to provide a read/write key via WithMasterKey(). This method is required unless you calling WithMocks().
func (r *AzureSharedResource) WithMasterKey(val string) *AzureSharedResource {
	if ablm, ok := r.leaseManager.(*azureBlobLeaseManager); ok {
		ablm.withMasterKey(val)
	}
	return r
}

// You may provide a factor that determines how much capacity each partition is worth. For instance, if you provision a Cosmos database
// with 20k RU, you might use a factor of 1000, meaning 20 partitions would be created, each worth 1k RU. If not provided, the factor
// defaults to `1`. There is a limit of 500 partitions, so if you have a shared capacity in excess of 500, you must provide a factor.
func (r *AzureSharedResource) WithFactor(val uint32) *AzureSharedResource {
	r.factor = val
	return r
}

// You may provide a reserved capacity. The capacity is always available to the rate limiter and is in addition to the shared capacity.
// For instance, if you have 4 processes and provision a Cosmos database with 28k RU, you might give each process 2,000 reserved capacity
// and 20,000 shared capacity. Any of the processes could obtain a maximum of 22,000 capacity. Capacity is renewed every 1 second.
// Generally you use reserved capacity to reduce your latency - you no longer have to wait on a partition to be acquired in order to
// process a small number of records.
func (r *AzureSharedResource) WithReservedCapacity(val uint32) *AzureSharedResource {
	r.reservedCapacity = val
	return r
}

// The rate limiter will attempt to obtain an exclusive lease on a partition (when needed) every so often. The interval is random to
// reduce the number of collisions and to provide an equal opportunity for processes to compete for partitions. This setting determines
// the maximum amount of time between intervals. It defaults to `500` and is measured in milliseconds.
func (r *AzureSharedResource) WithMaxInterval(val uint32) *AzureSharedResource {
	r.maxInterval = val
	return r
}

// Call this method before calling Start() to provision any needed partition blobs in the configured Azure Storage Account and container.
func (r *AzureSharedResource) Provision(ctx context.Context) (err error) {

	// only allow one phase at a time
	r.phaseMutex.Lock()
	defer r.phaseMutex.Unlock()
	if r.phase != rateLimiterPhaseUninitialized {
		err = RateLimiterImproperOrderError{}
		return
	}

	// check requirements
	if r.leaseManager == nil {
		err = UndefinedLeaseManagerError{}
		return
	}
	if r.factor == 0 {
		r.factor = 1 // assume 1:1
	}
	if r.sharedCapacity < 1 {
		err = UndefinedSharedCapacityError{}
		return
	}
	if r.maxInterval < 1 {
		r.maxInterval = 500 // default to 500 ms
	}

	// get a write lock on partitions
	r.partlock.Lock()
	defer r.partlock.Unlock()

	// provision the container
	err = r.leaseManager.provision(ctx)
	if err != nil {
		return
	}

	// make 1 partition per factor
	count := int(math.Ceil(float64(r.sharedCapacity) / float64(r.factor)))
	if count > 500 {
		err = PartitionsOutOfRangeError{
			MaxCapacity:    r.MaxCapacity(),
			Factor:         r.factor,
			PartitionCount: count,
		}
		return
	}
	r.partitions = make([]*string, count)

	// provision partitions
	err = r.leaseManager.createPartitions(ctx, count)

	// mark provision as completed
	r.phase = rateLimiterPhaseProvisioned

	return
}

// This returns the maximum capacity that could ever be obtained by the rate limiter. It is `SharedCapacity + ReservedCapacity`.
func (r *AzureSharedResource) MaxCapacity() uint32 {
	return r.sharedCapacity + r.reservedCapacity
}

// This returns the current allocated capacity. It is `NumberOfPartitionsControlled x Factor + ReservedCapacity`.
func (r *AzureSharedResource) Capacity() uint32 {
	allocatedCapacity := atomic.LoadUint32(&r.capacity)
	return allocatedCapacity + r.reservedCapacity
}

func (r *AzureSharedResource) calc() (total uint32) {

	// get a read lock
	r.partlock.RLock()
	defer r.partlock.RUnlock()

	// count the allocated partitions
	for i := 0; i < len(r.partitions); i++ {
		if r.partitions[i] != nil {
			total++
		}
	}

	// multiple by the factor
	total *= r.factor

	// set the capacity variable
	atomic.StoreUint32(&r.capacity, total)

	return
}

// You should call GiveMe() to update the capacity you are requesting. You will always specify the new amount of capacity you require.
// For instance, if you have a large queue of records to process, you might call GiveMe() every time new records are added to the queue
// and every time a batch is completed. Another common pattern is to call GiveMe() on a timer to keep it generally consistent with the
// capacity you need.
func (r *AzureSharedResource) GiveMe(target uint32) {

	// reduce capacity request by reserved capacity
	if target >= r.reservedCapacity {
		target -= r.reservedCapacity
	} else {
		target = 0
	}

	// determine the number of partitions needed
	actual := math.Ceil(float64(target) / float64(r.factor))

	// raise event
	r.emit(TargetEvent, int(target), "", nil)

	// store
	atomic.StoreUint32(&r.target, uint32(actual))

}

func (r *AzureSharedResource) getAllocatedAndRandomUnallocatedPartition() (count, index uint32, err error) {

	// get a read lock
	r.partlock.RLock()
	defer r.partlock.RUnlock()

	// get the list of unallocated
	unallocated := make([]uint32, 0)
	for i := 0; i < len(r.partitions); i++ {
		if r.partitions[i] == nil {
			unallocated = append(unallocated, uint32(i))
		} else {
			count++
		}
	}

	// make sure there is at least 1 unallocated
	len := len(unallocated)
	if len < 1 {
		err = fmt.Errorf("all partitions are already allocated")
		return
	}

	// pick a random partition
	i := rand.Intn(len)
	index = unallocated[i]

	return
}

func (r *AzureSharedResource) setPartitionId(index uint32, id string) {

	// get a write lock
	r.partlock.Lock()
	defer r.partlock.Unlock()

	// set the id
	r.partitions[index] = &id

}

func (r *AzureSharedResource) clearPartitionId(index uint32) {

	// get a write lock
	r.partlock.Lock()
	defer r.partlock.Unlock()

	// clear the id
	r.partitions[index] = nil

}

// Call this method to start the processing loop. It must be called after Provision(). The processing loop runs on a random interval
// not to exceed MaxInterval and attempts to obtain an exclusive lease on blob partitions to fulfill the capacity requests.
func (r *AzureSharedResource) Start(ctx context.Context) (err error) {

	// only allow one phase at a time
	r.phaseMutex.Lock()
	defer r.phaseMutex.Unlock()
	if r.phase != rateLimiterPhaseProvisioned {
		err = RateLimiterImproperOrderError{}
		return
	}

	// calculate capacity change
	recalc := func() {
		go func() {
			capacity := r.calc()
			r.emit(CapacityEvent, int(capacity+r.reservedCapacity), "", nil)
		}()
	}

	// announce starting capacity
	recalc()

	// prepare for shutdown
	r.shutdown.Add(1)
	r.stop = make(chan bool)

	// run the loop to try and allocate resources
	go func() {

		// shutdown
		defer func() {
			r.emit(ShutdownEvent, 0, "", nil)
			r.shutdown.Done()
		}()

	Loop:
		for {

			// check for a stop
			select {
			case <-r.stop:
				return
			default:
				// continue
			}

			// sleep for a bit before trying to obtain a new lease
			interval := rand.Intn(int(r.maxInterval))
			time.Sleep(time.Duration(interval) * time.Millisecond)

			// see how many partitions are allocated and if there any that can be allocated
			count, index, err := r.getAllocatedAndRandomUnallocatedPartition()
			target := atomic.LoadUint32(&r.target)
			if err == nil && count < target {

				// attempt to allocate the partition
				id := fmt.Sprint(uuid.New())
				leaseTime := r.leaseManager.leasePartition(ctx, id, index)
				if leaseTime == 0 {
					continue Loop
				}

				// clear the partition after the lease
				go func(i uint32) {
					time.Sleep(leaseTime)
					r.clearPartitionId(i)
					r.emit(ReleasedEvent, int(index), "", nil)
					recalc()
				}(index)

				// mark the partition as allocated
				r.setPartitionId(index, id)
				r.emit(AllocatedEvent, int(index), "", nil)
				recalc()

			}

		}
	}()

	// increment phase
	r.phase = rateLimiterPhaseStarted

	return
}

// Call this method to stop the processing loop. You may not restart after stopping.
func (r *AzureSharedResource) Stop() {

	// only allow one phase at a time
	r.phaseMutex.Lock()
	defer r.phaseMutex.Unlock()
	if r.phase == rateLimiterPhaseStopped {
		// NOTE: there should be no need for callers to handle errors at Stop(), we will just ignore them
		return
	}

	// signal the stop
	if r.stop != nil {
		close(r.stop)
	}
	r.shutdown.Wait()

	// update the phase
	r.phase = rateLimiterPhaseStopped

}
