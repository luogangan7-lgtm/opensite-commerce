-- 003 / Shared catalog, SKU option combinations and separately published copy.
CREATE TABLE app.products (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL DEFAULT uuidv7(),
 product_code text NOT NULL CHECK (btrim(product_code)<>''),
 product_code_normalized text GENERATED ALWAYS AS (upper(btrim(product_code))) STORED,
 lifecycle_status text NOT NULL DEFAULT 'draft'
   CHECK (lifecycle_status IN ('draft','ready','archived')),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,id),
 UNIQUE (tenant_id,product_code_normalized)
);
CREATE TABLE app.product_options (
 tenant_id uuid NOT NULL,
 product_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 code text NOT NULL CHECK (btrim(code)<>''),
 PRIMARY KEY (tenant_id,product_id,id),
 UNIQUE (tenant_id,product_id,code),
 FOREIGN KEY (tenant_id,product_id) REFERENCES app.products(tenant_id,id)
);
CREATE TABLE app.product_option_values (
 tenant_id uuid NOT NULL,
 product_id uuid NOT NULL,
 option_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 value_code text NOT NULL CHECK (btrim(value_code)<>''),
 label text NOT NULL,
 PRIMARY KEY (tenant_id,product_id,option_id,id),
 UNIQUE (tenant_id,product_id,option_id,value_code),
 FOREIGN KEY (tenant_id,product_id,option_id)
  REFERENCES app.product_options(tenant_id,product_id,id)
);
CREATE TABLE app.product_variants (
 tenant_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 product_id uuid NOT NULL,
 sku text NOT NULL CHECK (btrim(sku)<>''),
 sku_normalized text GENERATED ALWAYS AS (upper(btrim(sku))) STORED,
 option_key text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,id),
 UNIQUE (tenant_id,product_id,id),
 UNIQUE (tenant_id,sku_normalized),
 UNIQUE (tenant_id,product_id,option_key),
 FOREIGN KEY (tenant_id,product_id) REFERENCES app.products(tenant_id,id)
);
CREATE TABLE app.variant_option_values (
 tenant_id uuid NOT NULL,
 product_id uuid NOT NULL,
 variant_id uuid NOT NULL,
 option_id uuid NOT NULL,
 value_id uuid NOT NULL,
 PRIMARY KEY (tenant_id,product_id,variant_id,option_id),
 FOREIGN KEY (tenant_id,product_id,variant_id)
  REFERENCES app.product_variants(tenant_id,product_id,id),
 FOREIGN KEY (tenant_id,product_id,option_id,value_id)
  REFERENCES app.product_option_values(tenant_id,product_id,option_id,id)
);
CREATE TABLE app.attribute_definitions (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL DEFAULT uuidv7(),
 code text NOT NULL,
 value_type text NOT NULL CHECK (value_type IN ('text','integer','decimal','boolean','enum','measurement')),
 schema jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(schema)='object'),
 PRIMARY KEY (tenant_id,id),
 UNIQUE (tenant_id,code)
);
CREATE TABLE app.product_attribute_values (
 tenant_id uuid NOT NULL,
 product_id uuid NOT NULL,
 attribute_id uuid NOT NULL,
 text_value text,
 number_value numeric,
 bool_value boolean,
 enum_value text,
 unit_code text,
 PRIMARY KEY (tenant_id,product_id,attribute_id),
 FOREIGN KEY (tenant_id,product_id) REFERENCES app.products(tenant_id,id),
 FOREIGN KEY (tenant_id,attribute_id) REFERENCES app.attribute_definitions(tenant_id,id),
 CHECK (num_nonnulls(text_value,number_value,bool_value,enum_value)=1)
);
CREATE TABLE app.product_content_revisions (
 tenant_id uuid NOT NULL,
 product_id uuid NOT NULL,
 locale text NOT NULL,
 revision bigint NOT NULL CHECK (revision>0),
 title text NOT NULL CHECK (btrim(title)<>''),
 body_blocks jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(body_blocks)='array'),
 seo jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(seo)='object'),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,product_id,locale,revision),
 FOREIGN KEY (tenant_id,product_id) REFERENCES app.products(tenant_id,id)
);
CREATE TABLE app.product_listings (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 product_id uuid NOT NULL,
 locale text NOT NULL,
 slug text NOT NULL CHECK (btrim(slug)<>''),
 status text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','published','unpublished')),
 published_content_revision bigint,
 PRIMARY KEY (tenant_id,site_id,product_id,locale),
 UNIQUE (tenant_id,site_id,locale,slug),
 FOREIGN KEY (tenant_id,site_id) REFERENCES app.sites(tenant_id,id),
 FOREIGN KEY (tenant_id,product_id) REFERENCES app.products(tenant_id,id),
 FOREIGN KEY (tenant_id,product_id,locale,published_content_revision)
  REFERENCES app.product_content_revisions(tenant_id,product_id,locale,revision),
 CHECK (status <> 'published' OR published_content_revision IS NOT NULL)
);
CREATE INDEX product_variants_by_product ON app.product_variants(tenant_id,product_id);
CREATE INDEX product_listings_by_site ON app.product_listings(tenant_id,site_id,locale,status);
CREATE INDEX product_attributes_by_attribute ON app.product_attribute_values(tenant_id,attribute_id,enum_value);
