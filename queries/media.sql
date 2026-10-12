-- Media owns metadata; LocalFS content keys never derive from filenames.
-- name: ReserveMediaUpload :one
INSERT INTO app.media_uploads (tenant_id,original_filename,declared_mime,expected_size,expected_sha256,expires_at)
VALUES ($1,$2,$3,$4,$5,now()+interval '1 hour') RETURNING *;

-- name: LockMediaUpload :one
SELECT * FROM app.media_uploads WHERE tenant_id=$1 AND id=$2 FOR UPDATE;

-- name: MediaUploadExpired :one
-- Run AFTER LockMediaUpload in the same transaction. Expiry is judged by the database
-- clock (which stamped expires_at) at this instant: now() is the transaction start and
-- a same-statement projection is computed before a lock wait, so both can be stale.
SELECT expires_at <= clock_timestamp() FROM app.media_uploads WHERE tenant_id=$1 AND id=$2;

-- name: StoreMediaUploadContent :exec
UPDATE app.media_uploads SET temporary_key=$3, received_bytes=$4,status='stored' WHERE tenant_id=$1 AND id=$2;

-- name: CompleteMediaUploadRecord :exec
UPDATE app.media_uploads SET status='ready',media_id=$3 WHERE tenant_id=$1 AND id=$2;

-- name: UpsertReadyMediaBlob :one
-- The caller has just verified the bytes at the LocalFS key. A conflicting READY
-- row is reused unchanged; a STAGING row is taken over with the verified key; a
-- DELETING row is never revived (no row returned -> caller reports retryable 409).
INSERT INTO app.media_blobs (tenant_id,backend,storage_key,sha256,size_bytes,status)
VALUES ($1,'local',$2,$3,$4,'ready')
ON CONFLICT (tenant_id,sha256,size_bytes) DO UPDATE SET
 backend = CASE WHEN media_blobs.status='ready' THEN media_blobs.backend ELSE EXCLUDED.backend END,
 storage_key = CASE WHEN media_blobs.status='ready' THEN media_blobs.storage_key ELSE EXCLUDED.storage_key END,
 status = 'ready'
WHERE media_blobs.status IN ('ready','staging')
RETURNING *;

-- name: CreateMediaAsset :one
INSERT INTO app.media_assets (tenant_id,blob_id,filename,mime_type,width,height)
VALUES ($1,$2,$3,$4,$5,$6) RETURNING *;

-- name: LockMediaAsset :one
SELECT * FROM app.media_assets WHERE tenant_id=$1 AND id=$2 FOR UPDATE;

-- name: MediaProductStatus :one
SELECT lifecycle_status FROM app.products WHERE tenant_id=$1 AND id=$2;

-- name: LinkProductMedia :exec
INSERT INTO app.product_media (tenant_id,product_id,media_id,role,sort_order)
VALUES ($1,$2,$3,$4,$5) ON CONFLICT (tenant_id,product_id,media_id,role) DO UPDATE SET sort_order=EXCLUDED.sort_order;

-- name: DeleteProductMediaLinks :exec
DELETE FROM app.product_media WHERE tenant_id=$1 AND media_id=$2;

-- name: DeleteMediaAsset :exec
DELETE FROM app.media_assets WHERE tenant_id=$1 AND id=$2;
