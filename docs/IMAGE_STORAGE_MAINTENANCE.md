# Gallery performance and storage retention

## Retention rules

Can be set in the process environment or `.env`:

```dotenv
IMAGE_ASSET_RETENTION_DAYS=3
IMAGE_JOB_RETENTION_DAYS=30
```

Unset or 0 means the corresponding automatic deletion is not enabled. The job retention period must not be shorter than the image retention period; enabling job cleanup requires image cleanup to be enabled too. The first run happens 1 minute after startup, then once an hour; each run is limited to 5 minutes, and deletes at most 1000 expired images and 1000 expired job records, to avoid locking the database for a long time.

- Images are timed by creation time (UTC cutoff: a full 3×24 hours; an image exactly at the boundary but not past it is not deleted). Only the generated results of finished jobs are cleaned up, including the corresponding disk thumbnails and in-memory thumbnail cache. The external OSS objects behind the original reference image URLs are outside the scope of this cleanup.
- Files/objects are deleted through the existing gallery storage backend. If deletion fails, the metadata is kept and retried in the next round; the record is never deleted before the file location is lost.
- Jobs are timed by completion time, and only succeeded/failed jobs with no associated images left are deleted. queued/running jobs are always kept.
- For finished jobs older than the image retention period, the Base64 and internal temporary reference-image references in their historical parameters are removed, while the prompt and other parameters are kept. Old parameters are processed one entry at a time, so hundreds of MB of history are not loaded into memory at once.
- UUID temporary files in `queue-inputs` that are older than 24 hours and not referenced by an active job are cleaned up automatically. If reading the active references fails, no file is deleted; symbolic links are not followed, and other directories are not scanned recursively.
- After cleanup, old download links stop working. Images that the business has already copied to its own OSS are not affected.

Cleanup results are logged as `[image-retention]` with the number deleted and the bytes of generated images reclaimed. In SQLite, pages freed after deletion can be reused by new data, but the database file does not necessarily shrink immediately; VACUUM is not run on the running request path. Deployment backups are outside the scope of gallery cleanup, and backup retention must be managed separately. A 3-day retention does not guarantee sufficient disk capacity; it should be estimated as daily count × average image size × days.

## Gallery and history lists

The admin and public portal task list and task detail support `summary=1`:

- Input images and cached Base64 are not returned; only the necessary text settings are kept in the parameters.
- Old job parameters larger than 64 KiB are not loaded into the list response (an empty parameter object is returned). When a user explicitly clicks regenerate, the UI requests the full detail separately, so the original generation options are not lost.
- The images linked to the current page are fetched with one batch query, to avoid querying the database once per job. The portal is still isolated by API key.
- The original endpoint behavior is kept by default; the UI already uses the summary query, and when `summary=1` and `include_cache=1` appear together, Base64 image content is still not returned.
- The list uses thumbnails and lazy loading; the original image is used only when the preview is opened. Polling avoids overlapping requests, and queries pause while the page is in a background browser tab.

The thumbnail cache is written to `IMAGE_ASSET_DIR/.thumbnails/<asset-id>/`, and can be reused after in-process cache eviction or a restart. The first thumbnail generation runs at most 1 at a time, to avoid the gallery requests decoding many large images at once; when a thumbnail cannot be generated, the old endpoint's streamed original-image fallback is kept. The default 24-hour signed link is stable per hour (valid for about 23–24 hours), to improve the browser cache hit rate.

Global cleanup uses two query indexes. The optional [caller storage policy](IMAGE_RESULT_STORAGE.md) also adds the `image_assets.expires_at` and `delete_after_read` columns with an expiry index, and historical records still follow the global policy by default. Images with an explicitly set retention period are cleaned up separately every minute. Back up the database and gallery files before enabling cleanup.
