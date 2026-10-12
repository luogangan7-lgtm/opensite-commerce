-- 006 / Immutable releases, locale-aware routes, mutable drafts.
CREATE TABLE app.site_releases (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 release_no bigint NOT NULL CHECK (release_no>0),
 renderer_build_id text NOT NULL,
 manifest jsonb NOT NULL CHECK (jsonb_typeof(manifest)='object'),
 manifest_hash bytea NOT NULL CHECK (octet_length(manifest_hash)=32),
 restored_from uuid,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,site_id,id),
 UNIQUE (tenant_id,site_id,release_no),
 FOREIGN KEY (tenant_id,site_id) REFERENCES app.sites(tenant_id,id),
 FOREIGN KEY (tenant_id,site_id,restored_from)
  REFERENCES app.site_releases(tenant_id,site_id,id)
);
CREATE TABLE app.site_release_routes (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 release_id uuid NOT NULL,
 locale text NOT NULL,
 path text NOT NULL CHECK (left(path,1)='/'),
 page_id uuid NOT NULL,
 page_revision bigint NOT NULL,
 PRIMARY KEY (tenant_id,site_id,release_id,locale,path),
 FOREIGN KEY (tenant_id,site_id,release_id)
  REFERENCES app.site_releases(tenant_id,site_id,id),
 FOREIGN KEY (tenant_id,site_id,page_id,locale,page_revision)
  REFERENCES app.page_revisions(tenant_id,site_id,page_id,locale,revision)
);
CREATE TABLE app.site_release_templates (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 release_id uuid NOT NULL,
 locale text NOT NULL,
 template_kind text NOT NULL CHECK (template_kind IN ('product','blog','collection')),
 document jsonb NOT NULL CHECK (jsonb_typeof(document)='object'),
 PRIMARY KEY (tenant_id,site_id,release_id,locale,template_kind),
 FOREIGN KEY (tenant_id,site_id,release_id)
  REFERENCES app.site_releases(tenant_id,site_id,id)
);
CREATE TABLE app.site_release_media (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 release_id uuid NOT NULL,
 media_id uuid NOT NULL,
 PRIMARY KEY (tenant_id,site_id,release_id,media_id),
 FOREIGN KEY (tenant_id,site_id,release_id)
  REFERENCES app.site_releases(tenant_id,site_id,id),
 FOREIGN KEY (tenant_id,media_id) REFERENCES app.media_assets(tenant_id,id)
);
ALTER TABLE app.sites ADD CONSTRAINT sites_published_release_fkey
 FOREIGN KEY (tenant_id,id,published_release_id)
 REFERENCES app.site_releases(tenant_id,site_id,id);
CREATE TABLE app.site_drafts (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 base_release_id uuid,
 revision bigint NOT NULL DEFAULT 1 CHECK (revision>0),
 status text NOT NULL DEFAULT 'open' CHECK (status IN ('open','published','abandoned')),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,site_id,id),
 FOREIGN KEY (tenant_id,site_id) REFERENCES app.sites(tenant_id,id),
 FOREIGN KEY (tenant_id,site_id,base_release_id)
  REFERENCES app.site_releases(tenant_id,site_id,id)
);
CREATE TABLE app.draft_pages (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 draft_id uuid NOT NULL,
 page_id uuid NOT NULL,
 locale text NOT NULL,
 path text NOT NULL CHECK (left(path,1)='/'),
 document jsonb NOT NULL CHECK (jsonb_typeof(document)='object'),
 PRIMARY KEY (tenant_id,site_id,draft_id,page_id,locale),
 UNIQUE (tenant_id,site_id,draft_id,locale,path),
 FOREIGN KEY (tenant_id,site_id,draft_id)
  REFERENCES app.site_drafts(tenant_id,site_id,id),
 FOREIGN KEY (tenant_id,site_id,page_id)
  REFERENCES app.pages(tenant_id,site_id,id)
);
CREATE TABLE app.draft_operations (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 draft_id uuid NOT NULL,
 operation_id uuid NOT NULL,
 actor_id uuid NOT NULL,
 before_revision bigint NOT NULL,
 after_revision bigint NOT NULL,
 operation jsonb NOT NULL CHECK (jsonb_typeof(operation)='object'),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,site_id,draft_id,operation_id),
 UNIQUE (tenant_id,site_id,draft_id,after_revision),
 FOREIGN KEY (tenant_id,site_id,draft_id)
  REFERENCES app.site_drafts(tenant_id,site_id,id),
 CHECK (after_revision=before_revision+1)
);
