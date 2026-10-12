-- Media owns metadata; LocalFS content keys never derive from filenames.
-- name: ReserveMediaUpload :one
INSERT INTO app.media_uploads (tenant_id,original_filename,declared_mime,expected_size,expected_sha256,expires_at)
VALUES ($1,$2,$3,$4,$5,now()+interval '1 hour') RETURNING *;

-- name: LockMediaUpload :one
SELECT * FROM app.media_uploads WHERE tenant_id=$1 AND id=$2 FOR UPDATE;

-- name: StoreMediaUploadContent :exec
UPDATE app.media_uploads SET temporary_key=$3, received_bytes=$4,status='stored' WHERE tenant_id=$1 AND id=$2;

-- name: CompleteMediaUploadRecord :exec
UPDATE app.media_uploads SET status='ready',media_id=$3 WHERE tenant_id=$1 AND id=$2;

-- name: UpsertReadyMediaBlob :one
INSERT INTO app.media_blobs (tenant_id,backend,storage_key,sha256,size_bytes,status)
VALUES ($1,'local',$2,$3,$4,'ready')
ON CONFLICT (tenant_id,sha256,size_bytes) DO UPDATE SET status='ready'
RETURNING *;

-- name: CreateMediaAsset :one
INSERT INTO app.media_assets (tenant_id,blob_id,filename,mime_type,width,height)
VALUES ($1,$2,$3,$4,$5,$6) RETURNING *;

-- name: LockMediaAsset :one
SELECT * FROM app.media_assets WHERE tenant_id=$1 AND id=$2 FOR UPDATE;

-- name: MediaProductExists :one
SELECT EXISTS(SELECT FROM app.products WHERE tenant_id=$1 AND id=$2);

-- name: LinkProductMedia :exec
INSERT INTO app.product_media (tenant_id,product_id,media_id,role,sort_order)
VALUES ($1,$2,$3,$4,$5) ON CONFLICT (tenant_id,product_id,media_id,role) DO UPDATE SET sort_order=EXCLUDED.sort_order;

-- name: DeleteProductMediaLinks :exec
DELETE FROM app.product_media WHERE tenant_id=$1 AND media_id=$2;

-- name: DeleteMediaAsset :exec
DELETE FROM app.media_assets WHERE tenant_id=$1 AND id=$2;
