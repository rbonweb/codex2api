# Persistent image job queue

After setting `IMAGE_JOB_WORKERS=2` and restarting the app, a fixed number of image workers is enabled. When it is unset or `0` (the default), the old job intake method is kept. The valid range is 0–32. This feature targets single-instance deployments; the concurrency limit is counted per process, not as a cluster-wide limit across replicas.

## Intake and execution

- The external `/v1/images/jobs`, the admin workbench and the public workbench share the database queue. API key authentication, prompt checks and quota checks still apply; the concurrency slot is now acquired at execution time.
- Workers take jobs round-robin across API keys: each scan looks only at the oldest queued job of each key, and takes that key's concurrency slot before claiming it. If a key's concurrency is full because of its own requests (such as long streaming connections), its jobs stay in the queue and the worker runs jobs from other keys instead of waiting in place.
- A single API key can have at most `IMAGE_JOB_MAX_QUEUED_PER_KEY` queued jobs (default 1000, `0` means no limit). Beyond that, the create endpoint returns 429 with `Retry-After`.
- A job that has waited longer than `IMAGE_JOB_MAX_QUEUE_WAIT` without starting is marked failed, and its persisted reference images are cleaned up (default `24h`, `0` disables it, minimum `1m`, Go duration format). Such jobs never reached upstream and can be retried directly.
- Reference image URLs are stored as strings only at enqueue time: no remote server is contacted and nothing is converted to Base64. The existing download-address safety check is kept and runs when the worker executes.
- data URL uploads are first written to `IMAGE_ASSET_DIR/queue-inputs` (default `/data/images/queue-inputs`); the database stores only an internal file ID. Temporary files are cleaned up when execution finishes normally. This directory must be persisted; do not place it in the container layer, which disappears on rebuild.
- The external create endpoint returns a small receipt `{"job":{"id":42,"status":"queued"}}`. The old detail query keeps its original fields, and the summary query is still `/v1/images/jobs/:id/result`. Generated images still use the existing gallery files/object storage and `assets[].proxy_url`.
- A worker reads inputs, calls upstream, waits for the result and saves the image only after it has claimed the job. No long-lived goroutine is created per queued job, and the parameters of the whole queue are never loaded at once.
- `/v1/images/generations` and `/v1/images/edits` share the in-process execution slots with the workers; when busy they return 429 before the request body is parsed. Clients that need to queue should use the jobs endpoint. This limit does not cover image tool calls that clients build themselves in the general Responses API.
- A single job's `n` is still 1–4, and the images are generated one after another. For a batch of 50 images, create multiple jobs. Queue capacity is limited by disk, quota and database capacity; it is not unlimited.

## Memory bounds

At most 2 enqueue requests are parsed at the same time, and a request body is at most 48 MiB. A single reference image is at most 20 MiB, all reference images together at most 32 MiB, and the pixel count declared in an image header at most 40 million. Inputs are validated again before execution; in queue mode, the pixel count of generated results is also checked before the expensive upscale operation.

Caches can be adjusted separately with `IMAGE_UPSCALE_CACHE_MB` (1–4096, default 512) and `IMAGE_THUMB_CACHE_MB` (1–1024, default 64). For a 2 GiB machine, start validation with 2 workers, a 64 MiB upscale cache, a 16 MiB thumbnail cache and `GOMEMLIMIT=768MiB`. The Go memory target is a soft target; memory must also be reserved for the OS, other services, caches and build processes. Swap is a peak buffer and cannot replace execution limits.

## Database and restarts

On first enablement, the following is idempotently added to the existing `image_generation_jobs` table:

| Column | Type / default | Purpose |
| --- | --- | --- |
| `queue_owner` | TEXT / empty string | Unique identifier of this claim |
| `queue_lease_until` | BIGINT / 0 | Unix seconds when the lease expires |

Indexes on `(status, id)` and `(status, api_key_id, id)` are also added. The add-column syntax is supported on SQLite/PostgreSQL; no tables, fields, historical images or API keys are deleted. Back up the database and image directory before release.

Job claims use a conditional update, so only one worker can claim a job successfully. The lease lasts 2 minutes and is renewed every 20 seconds. Terminal-state writes check ownership and the lease, to stop a stale worker from overwriting recovered results.

After a restart, `queued` jobs are kept and continue to run. A `running` job whose lease has expired is marked failed, clearly stating that the upstream result is unknown, and **the generation request is not submitted again automatically**. Running jobs from the old version that have no lease are likewise marked interrupted on first enablement. A running record whose lease has not yet expired is handled on the queue's next scan; if all workers are busy, the scan is delayed. Check upstream and billing first, then decide whether to retry manually.

Historical images from jobs that already ran are not affected. When rolling back the binary, the new columns can be kept, but the pending queue must be handled first: the startup logic of the old version marks queued/running jobs as interrupted. Multi-replica deployments also need a cluster-wide concurrency limit and shared input-file storage to be designed.


## Staged processing to release memory after upload (optional)

After setting `IMAGE_JOB_WORKERS=10` and `IMAGE_JOB_MEMORY_WORKERS=1` and restarting: up to 10 jobs are in flight, but only 1 job at a time may be preparing reference images, building or compressing requests, or processing returned results. Once a request has been turned into a replayable disk file, its memory-processing slot is released, and other jobs can keep sending without waiting for the previous job's generation to finish. The number that actually reaches upstream at the same time is still limited by accounts, API keys, the scheduler and upstream quota.

When `IMAGE_JOB_MEMORY_WORKERS` is unset or 0, the original worker implementation is kept and the default behavior does not change; the valid range is 0..IMAGE_JOB_WORKERS. Direct calls to images/generations/edits still share the in-flight limit, and conservatively hold a memory-processing slot until they finish; for batch calls, the jobs endpoint is recommended. The general Responses API is still outside this dedicated limit.

- Request building and compression are rate limited; the final HTTP request body is streamed from a private file, retries replay from the file, and model fallback and account scheduling are kept. The compressor uses an independent single-threaded, small-window instance, to avoid the large amount of history buffer held by the general compressor cache.
- The input Base64 is not held while waiting for GPT; the response SSE is staged to disk in 32 KiB chunks (at most 128 MiB, and exceeding it fails explicitly without returning a truncated image). The memory slot is acquired to parse the result only after the terminal event is received.
- The Base64 encoding of each result is streamed-decoded to a file and handed directly to Local/S3 storage. When no resizing is required, or the size already meets the requirement, only the image header is read and the pixels are not expanded. The image is read in full only when size processing is actually needed, and that is subject to the memory slot limit.
- Multiple Base64 JSON results are no longer aggregated, nor are large images returned through an internal HTTP call; n>1 saves each image one by one, and the job status is updated at the end, keeping the logic of partial success and settlement by delivered images.
- Request, response and decode staging files are located at `IMAGE_ASSET_DIR/pipeline-tmp/job-*`, with permissions 0700/0600, and are cleaned up immediately when the job completes, fails or is cancelled. Directories left behind by abnormal exits are cleaned up by hourly maintenance once they are older than 24h. They are never kept permanently. Deployments must reserve staging disk space and protect the directory from being written by external processes.
- A local disk or save failure does not trigger an extra paid image generation; network failures follow the existing retry rules. Queue/job leases, API authentication and quota checks are unchanged, and there are no new database tables or fields.

Waiting for a slot is not unlimited concurrency, nor a throughput promise. For a 2 GB machine, start validation with memory processing at 1 and in-flight at 10; keep the runtime memory limit, and watch account rate limits, GC, swap and disk. Do not estimate peak usage from idle memory.
