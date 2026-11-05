# Image result storage policy

`POST /v1/images/jobs` supports the following optional parameters. The admin and portal jobs endpoints also support them; the protocol of the synchronous `/v1/images/generations` and `/v1/images/edits` is unchanged.

| Parameter | Default | Description |
| --- | --- | --- |
| `storage_mode` | `legacy` | `legacy`, `temporary`, `delete_after_read` |
| `retention_seconds` | In temporary mode, when omitted or `0`, `7200` | Retention in seconds after a successful save; a non-zero value must be 60–31536000. 2 hours is 7200, 3 days is 259200. Legacy mode cannot take a non-zero value. |

## Three modes

- **legacy / no parameters**: keeps the existing Local/S3 storage and the operator's global cleanup settings; when there is no global retention period, historical results are not deleted proactively.
- **temporary**: the expiry time is calculated from the moment each image is saved successfully; reads do not delete it early. Suitable when the caller needs to download, retry or recover an unfinished job.
- **delete_after_read**: the result is cleaned up after it has been completely written out through the Base64 delivery endpoint described below, or after the original image is downloaded and the acknowledgement endpoint is called. Results that are never retrieved still expire and are reclaimed according to `retention_seconds`.

```json
{
  "model": "gpt-image-2",
  "prompt": "A cat sleeping in sunlight",
  "n": 1,
  "storage_mode": "temporary",
  "retention_seconds": 7200
}
```

To delete on retrieval, change `storage_mode` to `delete_after_read`. To keep results for 3 days, change `retention_seconds` to `259200`. Omit both parameters to restore the old behavior.

Temporary mode still needs image files/objects saved, so asynchronous callers can retrieve them later; it does not write image Base64 to the database. The job parameters record the storage policy, and the image records gain `expires_at` (Unix seconds) and `delete_after_read`. The policy of an image is written together with its metadata; historical rows default to `0/false` and do not change the retention period of old images.

An explicit image retention period takes precedence over the global `IMAGE_ASSET_RETENTION_DAYS`, and is not deleted early by the global cleanup. A background task scans for expired images every minute, handling at most 1000 per run; a backlog, storage errors or service downtime may delay physical deletion. After expiry, requests for the original, thumbnail and inline cached images stop being served immediately. If deletion fails, the expired metadata is kept and cleanup continues next time.

## Query, delivery and acknowledgement

1. Poll status with the existing `GET /v1/images/jobs/:id/result` or the batch query. In the new modes, `assets[]` adds the optional `expires_at` and `delete_after_read` fields; old modes do not add these fields.
2. After the job completes, either download using the existing `assets[].proxy_url`, or read Base64 with `GET /v1/images/jobs/:id/output`. Both new endpoints require the same API key that created the job; other keys get 404.
3. A caller that downloads by URL and selects `delete_after_read` should call `POST /v1/images/jobs/:id/ack` after it has reliably saved the image itself. Success returns 204, and repeated acknowledgements are allowed. Only images marked `delete_after_read` for that job are deleted; legacy/temporary images and job history are not deleted.

`GET /v1/images/jobs/:id/output` returns:

```json
{
  "job_id": 42,
  "data": [{"id": 71, "mime_type": "image/png", "width": 1024, "height": 1024, "b64_json": "..."}]
}
```

This endpoint encodes images one by one as a stream and does not aggregate multiple Base64 values in memory. queued/running return 409; if the result has expired, has been cleaned up, or there is no deliverable image, it returns 410. A partially successful job returns the outputs that still exist; check the `warning` in the status query to tell whether all images were generated. A failed or interrupted write-out does not proactively clean up the remaining valid images.

A complete HTTP write-out does not prove that the client has persisted the data. If it is necessary to guarantee that cleanup happens only after the caller has saved successfully, use **original image URL download + ack**. Do not use the auto-consumable `/output` for unconditional automatic retries. Status polling, the old detail query, original image URLs and thumbnail previews never consume images automatically; automatic deletion happens only on an explicit delivery or acknowledgement. Expired or consumed images are not regenerated and do not trigger extra billing.

## Batch jobs and compatibility

`POST /v1/images/jobs/results` (compatibility alias `/result`) accepts `{"ids":[1,2,3]}`, and also accepts `job_ids`. At most 500 IDs and 32 KiB of request body; duplicate IDs are de-duplicated. IDs that are not found or do not belong to the current key are returned in `missing_ids`, and this cannot be used to tell whether a job belongs to someone else. The response contains no input images, prompts or Base64. 1000 jobs can be split into two batch queries.

1000 jobs should be queued for execution, not decoded at the same time on a 2 GiB server. The queue and memory-stage limits still need to be set explicitly, for example `IMAGE_JOB_WORKERS=10` and `IMAGE_JOB_MEMORY_WORKERS=1`, and the peak should be measured against image size and upstream quota. API key, account and scope concurrency limits continue to apply; when the queue is not enabled, the intake behavior of the old endpoints is unchanged.

The database migration only adds two columns and an expiry index, and supports SQLite/PostgreSQL. Rolling back to the old binary can keep the new columns, but the old program will not execute the new expiry/consumption policy, so these images must be cleaned up first or the cleanup must be arranged separately. Backups outside the application, S3 version history and the caller's own files are not deleted by this feature.
