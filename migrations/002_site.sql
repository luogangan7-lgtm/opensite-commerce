-- 002 / Organizations, sites and proof-of-control for domains.
CREATE TABLE app.organizations (
 tenant_id uuid NOT NULL REFERENCES app.tenants(id),
 id uuid NOT NULL DEFAULT uuidv7(),
 legal_name text,
 display_name text NOT NULL CHECK (btrim(display_name)<>''),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,id)
);
CREATE TABLE app.sites (
 tenant_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 organization_id uuid NOT NULL,
 name text NOT NULL CHECK (btrim(name)<>''),
 default_locale text NOT NULL DEFAULT 'en',
 published_release_id uuid,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,id),
 FOREIGN KEY (tenant_id,organization_id) REFERENCES app.organizations(tenant_id,id)
);
CREATE TABLE app.site_domains (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 id uuid NOT NULL DEFAULT uuidv7(),
 hostname text NOT NULL,
 normalized_hostname text GENERATED ALWAYS AS (lower(trim(trailing '.' FROM btrim(hostname)))) STORED,
 verification_token_hash bytea,
 verified_at timestamptz,
 status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','verified','disabled')),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (tenant_id,site_id,id),
 FOREIGN KEY (tenant_id,site_id) REFERENCES app.sites(tenant_id,id),
 CHECK (position('/' IN hostname)=0),
 CHECK (status <> 'verified' OR verified_at IS NOT NULL)
);
CREATE UNIQUE INDEX site_domains_verified_unique
 ON app.site_domains(normalized_hostname) WHERE status='verified';
CREATE TABLE app.site_locales (
 tenant_id uuid NOT NULL,
 site_id uuid NOT NULL,
 locale text NOT NULL,
 enabled boolean NOT NULL DEFAULT true,
 PRIMARY KEY (tenant_id,site_id,locale),
 FOREIGN KEY (tenant_id,site_id) REFERENCES app.sites(tenant_id,id)
);
