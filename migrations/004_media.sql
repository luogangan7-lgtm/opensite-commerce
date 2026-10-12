-- 004 / Files always live outside PostgreSQL; assets have stable database IDs.
CREATE TABLE app.media_uploads (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL DEFAULT uuidv7(),
 actor_id uuid,
 original_filename text NOT NULL,
 declared_mime text,
 expected_size bigint CHECK (expected_size >= 0),
 expected_sha256 bytea CHECK (expected_sha256 IS NULL OR octet_length(expected_sha256)=32),
 temporary_key text,
 received_bytes bigint NOT NULL DEFAULT 0 CHECK (received_bytes >= 0),
 status text NOT NULL DEFAULT 'reserved'
  CHECK (status IN ('reserved','uploading','verifying','stored','processing','ready','failed')),
 expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,id)
);
CREATE TABLE app.media_blobs (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL DEFAULT uuidv7(),
 backend text NOT NULL CHECK (backend IN ('local','s3')),
 storage_key text NOT NULL,
 sha256 bytea NOT NULL CHECK (octet_length(sha256)=32),
 size_bytes bigint NOT NULL CHECK (size_bytes>=0),
 status text NOT NULL DEFAULT 'staging' CHECK (status IN ('staging','ready','deleting')),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,id),
 UNIQUE (tenant_id,backend,storage_key),
 UNIQUE (tenant_id,sha256,size_bytes)
);
CREATE TABLE app.media_assets (
 tenant_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 blob_id uuid NOT NULL,
 filename text NOT NULL,
 mime_type text NOT NULL,
 visibility text NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','public')),
 width integer CHECK (width>0),
 height integer CHECK (height>0),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,id),
 FOREIGN KEY (tenant_id,blob_id) REFERENCES app.media_blobs(tenant_id,id)
);
CREATE TABLE app.product_media (
 tenant_id uuid NOT NULL,
 product_id uuid NOT NULL,
 media_id uuid NOT NULL,
 role text NOT NULL CHECK (role IN ('main','gallery','detail','video','size_chart')),
 sort_order integer NOT NULL DEFAULT 0,
 PRIMARY KEY (tenant_id,product_id,media_id,role),
 FOREIGN KEY (tenant_id,product_id) REFERENCES app.products(tenant_id,id),
 FOREIGN KEY (tenant_id,media_id) REFERENCES app.media_assets(tenant_id,id)
);
CREATE INDEX media_assets_by_blob ON app.media_assets(tenant_id,blob_id);
