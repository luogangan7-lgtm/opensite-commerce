-- 005 / Company, pages, blog and B2B inquiry. Public visibility is explicit.
CREATE TABLE app.company_contacts (
 tenant_id uuid NOT NULL,
 organization_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 contact_type text NOT NULL CHECK (contact_type IN ('email','phone','whatsapp','facebook','instagram','linkedin','youtube','tiktok','address')),
 contact_value text NOT NULL CHECK (btrim(contact_value)<>''),
 is_public boolean NOT NULL DEFAULT false,
 sort_order integer NOT NULL DEFAULT 0,
 PRIMARY KEY (tenant_id,organization_id,id),
 FOREIGN KEY (tenant_id,organization_id) REFERENCES app.organizations(tenant_id,id)
);
CREATE TABLE app.company_profile_revisions (
 tenant_id uuid NOT NULL,
 organization_id uuid NOT NULL,
 locale text NOT NULL,
 revision bigint NOT NULL CHECK (revision>0),
 profile jsonb NOT NULL CHECK (jsonb_typeof(profile)='object'),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,organization_id,locale,revision),
 FOREIGN KEY (tenant_id,organization_id) REFERENCES app.organizations(tenant_id,id)
);
CREATE TABLE app.pages (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 page_key text NOT NULL CHECK (btrim(page_key)<>''),
 PRIMARY KEY (tenant_id,site_id,id),
 UNIQUE (tenant_id,site_id,page_key),
 FOREIGN KEY (tenant_id,site_id) REFERENCES app.sites(tenant_id,id)
);
CREATE TABLE app.page_revisions (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 page_id uuid NOT NULL,
 locale text NOT NULL,
 revision bigint NOT NULL CHECK (revision>0),
 document jsonb NOT NULL CHECK (jsonb_typeof(document)='object'),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,site_id,page_id,locale,revision),
 FOREIGN KEY (tenant_id,site_id,page_id) REFERENCES app.pages(tenant_id,site_id,id)
);
CREATE TABLE app.blog_posts (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,site_id,id),
 FOREIGN KEY (tenant_id,site_id) REFERENCES app.sites(tenant_id,id)
);
CREATE TABLE app.blog_post_revisions (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 post_id uuid NOT NULL,
 locale text NOT NULL,
 revision bigint NOT NULL CHECK (revision>0),
 title text NOT NULL,
 excerpt text NOT NULL DEFAULT '',
 body_blocks jsonb NOT NULL CHECK (jsonb_typeof(body_blocks)='array'),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,site_id,post_id,locale,revision),
 FOREIGN KEY (tenant_id,site_id,post_id) REFERENCES app.blog_posts(tenant_id,site_id,id)
);
CREATE TABLE app.blog_post_publications (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 post_id uuid NOT NULL,
 locale text NOT NULL,
 slug text NOT NULL,
 published_revision bigint,
 status text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','published','unpublished')),
 PRIMARY KEY (tenant_id,site_id,post_id,locale),
 UNIQUE (tenant_id,site_id,locale,slug),
 FOREIGN KEY (tenant_id,site_id,post_id) REFERENCES app.blog_posts(tenant_id,site_id,id),
 FOREIGN KEY (tenant_id,site_id,post_id,locale,published_revision)
  REFERENCES app.blog_post_revisions(tenant_id,site_id,post_id,locale,revision),
 CHECK (status <> 'published' OR published_revision IS NOT NULL)
);
CREATE TABLE app.inquiries (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 product_id uuid,
 buyer_email text NOT NULL,
 buyer_name text,
 quantity numeric CHECK (quantity>0),
 message text NOT NULL CHECK (char_length(message) BETWEEN 1 AND 10000),
 status text NOT NULL DEFAULT 'new' CHECK (status IN ('new','contacted','quoted','closed')),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,site_id,id),
 FOREIGN KEY (tenant_id,site_id) REFERENCES app.sites(tenant_id,id),
 FOREIGN KEY (tenant_id,product_id) REFERENCES app.products(tenant_id,id)
);
CREATE INDEX inquiries_recent ON app.inquiries(tenant_id,site_id,created_at DESC,id DESC);
