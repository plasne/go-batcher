# Events

Events are raised with a "name" (string), "val" (int), and "msg" (*string).

## Events raised by Batcher

The following events can be raised by Batcher...

- __shutdown__: This is raised when the context provided to Start() is "done" (cancelled, deadlined, etc.).

- __pause__: This is raised after Pause() is called on a Batcher instance. The val is the number of milliseconds that it was paused for.

- __resume__: This is raised after a Pause() is complete.

- __audit-fail__: This is raised if an error was found during the AuditInterval. The msg contains more details. Should an audit fail, there is no additional action required, the Target will automatically be remediated.

- __audit-pass__: This is raised if the AuditInterval found no issues.

- __audit-skip__: If the Buffer is not empty or if MaxOperationTime (on Batcher) has not been exceeded by the last batch raised, the audit will be skipped. It is normal behavior to see lots of skipped audits.

- __request__: This is raised only when WithEmitRequest and a rate limiter has been added to Batcher. It is raised at the CapacityInterval with val containing the capacity being requested of the rate limiter. There is no security concern with event, it is disabled by default because it raises every 100ms by default.

- __flush-start__: This is raised only when WithEmitFlush has been added to Batcher. It is raised at the FlushInterval when the flush is started. There is no security concern with event, it is disabled by default because it raises every 100ms by default.

- __flush-done__: This is raised only when WithEmitFlush has been added to Batcher. It is raised at the FlushInterval when the flush is completed. There is no security concern with event, it is disabled by default because it raises every 100ms by default.

## Events raised by SharedResource

The following events can be raised by SharedResource or its associated LeaseManager...

- __shutdown__: This is raised when the context provided to Start() is "done" (cancelled, deadlined, etc.).

- __capacity__: This is raised anytime the Capacity changes. The val is the available capacity.

- __batch__: This is raised only when WithEmitBatch has been added to Batcher and whenever a batch is raised to any Watcher. The val is the count of the operations in the batch. The metadata contains an array of all Operations in the batch. Enabling this event creates a potential security issue as it would allow any block of code with access to the Batcher to see Operations for Watchers the code didn't create.

- __failed__: This is raised if the rate limiter fails to procure capacity. This does not indicate an error condition, it is expected that attempts to procure additional capacity will have failures. The val is the index of the partition that was not obtained.

- __released__: This is raised whenever the rate limiter releases capacity. The val is the index of the partition for which the lease was released.

- __allocated__: This is raised whenever the rate limiter gains capacity. The val is the index of the partition for which an exclusive lease was obtained.

- __target__: This is raised whenever the rate limiter is asked for capacity. The val is the number of partitions it will attempt to allocate to satisfy the capacity request. For example, if the Factor is 1,000 and the request is for 8,750, then the target event will be raised with a val of 9.

- __error__: This is raised if there was some unexpected error condition, such as an authentication failure when attempting to allocate a partition.

- __provision-start__: If SharedCapacity is used, there will be a provisioning activity at Start() and whenever the SharedCapacity changes. This event is raised at the start of that provisioning activity. The provisioning activity may raise events such as those shown below by AzureBlobLeaseManager.

- __provision-done__: This is raised at the end of provisioning activity after all other provisioning events are raised.

### Events raised when using AzureBlobLeaseManager

- __created-container__: This is raised if a container is created during a provisioning activity. The msg is the fully qualified path to the container.

- __verified-container__: This is raised if a container was found to already exist during a provisioning activity. The msg is the fully qualified path to the container.

- __created-blob__: This is raised if a zero-byte blob needs to be created for a partition. The val is the index of the partition created.

- __verified-blob__: This is raised if a zero-byte blob partition was found to already exist. The val is the index of the partition verified.
