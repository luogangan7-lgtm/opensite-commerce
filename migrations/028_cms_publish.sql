-- Responsibility: controlled CMS drafts and atomic, immutable publication.
-- Dependencies: frozen 005/006/010/024, catalog/pricing read-only; caller: CMS sqlc adapter.
-- Invariants: tenant+trusted actor GUCs, managed draft CAS, site ABA fence, sealed pins.
-- Legacy unmanaged drafts keep predecessor access; C09b never admits them.
ALTER TABLE app.sites ADD COLUMN publication_revision bigint NOT NULL DEFAULT 0 CHECK(publication_revision>=0);
-- Per-revision media attribution; NULL marks pre-CMS revisions (release-level pins only).
ALTER TABLE app.page_revisions ADD COLUMN media_ids uuid[] CHECK(cardinality(media_ids)<=100);
ALTER TABLE app.site_drafts ADD COLUMN is_cms boolean NOT NULL DEFAULT false,
 ADD COLUMN locale text NOT NULL DEFAULT 'en', ADD COLUMN title text NOT NULL DEFAULT '',
 ADD COLUMN description text NOT NULL DEFAULT '', ADD COLUMN navigation jsonb NOT NULL DEFAULT '[]' CHECK(jsonb_typeof(navigation)='array');
CREATE TABLE app.draft_page_media (
 tenant_id uuid NOT NULL,site_id uuid NOT NULL,draft_id uuid NOT NULL,page_id uuid NOT NULL,locale text NOT NULL,media_id uuid NOT NULL,
 PRIMARY KEY(tenant_id,site_id,draft_id,page_id,locale,media_id),
 FOREIGN KEY(tenant_id,site_id,draft_id,page_id,locale) REFERENCES app.draft_pages(tenant_id,site_id,draft_id,page_id,locale),
 FOREIGN KEY(tenant_id,media_id) REFERENCES app.media_assets(tenant_id,id)
);
ALTER TABLE app.draft_page_media ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.draft_page_media FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON app.draft_page_media TO app_runtime
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
CREATE POLICY publish_executor_scope ON app.draft_page_media TO app_publish_executor
 USING(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid)
 WITH CHECK(tenant_id=nullif(current_setting('app.tenant_id',true),'')::uuid);
GRANT SELECT ON app.draft_page_media TO app_runtime,app_publish_executor;
-- Table grants would otherwise permit setting the capability flag/publication epoch.
REVOKE INSERT,UPDATE ON app.site_drafts FROM app_runtime;
GRANT INSERT(tenant_id,site_id,id,base_release_id,revision,status,created_at),
 UPDATE(tenant_id,site_id,id,base_release_id,revision,status,created_at) ON app.site_drafts TO app_runtime;
REVOKE INSERT ON app.sites FROM app_runtime;
GRANT INSERT(tenant_id,id,organization_id,name,default_locale,created_at) ON app.sites TO app_runtime;
GRANT UPDATE(publication_revision) ON app.sites TO app_publish_executor;
GRANT SELECT ON app.site_drafts,app.draft_pages,app.draft_operations,app.pages,app.page_revisions,
 app.products,app.product_variants,app.product_listings,app.product_content_revisions,
 app.price_lists,app.price_entries TO app_publish_executor;
GRANT INSERT(tenant_id,site_id,id,base_release_id,revision,status,is_cms,locale,title,description,navigation) ON app.site_drafts TO app_publish_executor;
GRANT UPDATE(revision,status,locale,title,description,navigation) ON app.site_drafts TO app_publish_executor;
GRANT INSERT ON app.draft_pages,app.draft_page_media,app.pages,app.page_revisions,
 app.site_release_routes,app.site_release_media,app.draft_operations,app.audit_events TO app_publish_executor;
GRANT INSERT(tenant_id,site_id,id,release_no,renderer_build_id,manifest,manifest_hash) ON app.site_releases TO app_publish_executor;
GRANT UPDATE(path,document) ON app.draft_pages TO app_publish_executor;
GRANT DELETE ON app.draft_pages,app.draft_page_media TO app_publish_executor;
DO $policies$ DECLARE n text; BEGIN
 FOREACH n IN ARRAY ARRAY['site_drafts','draft_pages','draft_operations','pages','page_revisions','products','product_variants','product_listings','product_content_revisions','price_lists','price_entries','audit_events'] LOOP
  EXECUTE format('CREATE POLICY publish_executor_scope ON app.%I TO app_publish_executor USING(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid) WITH CHECK(tenant_id=nullif(current_setting(''app.tenant_id'',true),'''')::uuid)',n);
 END LOOP;
END $policies$;

-- Extend the existing sole pointer writer so legacy publishers also advance
-- the ABA epoch; frozen 010 remains untouched.
CREATE OR REPLACE FUNCTION app.publish_release(p_tenant uuid,p_site uuid,p_release uuid,p_expected uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $publish$
DECLARE previous uuid;
BEGIN
 IF current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'PUBLISH_READ_COMMITTED_REQUIRED' USING ERRCODE='25001';
 END IF;
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'PUBLISH_TENANT_REQUIRED' USING ERRCODE='42501';
 END IF;
 SELECT published_release_id INTO previous FROM app.sites
  WHERE tenant_id=p_tenant AND id=p_site FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'SITE_NOT_VISIBLE' USING ERRCODE='23503'; END IF;
 IF previous IS DISTINCT FROM p_expected THEN
  RAISE EXCEPTION 'PUBLISH_POINTER_CHANGED' USING ERRCODE='40001';
 END IF;
 PERFORM app.seal_release(p_tenant,p_site,p_release);
 UPDATE app.sites SET published_release_id=p_release,publication_revision=publication_revision+1 WHERE tenant_id=p_tenant AND id=p_site;
END $publish$;

CREATE FUNCTION app.cms_context(p_tenant uuid,p_kind text DEFAULT NULL,p_actor uuid DEFAULT NULL,p_read boolean DEFAULT false) RETURNS void
LANGUAGE plpgsql SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 IF p_tenant IS NULL OR p_tenant IS DISTINCT FROM nullif(current_setting('app.tenant_id',true),'')::uuid THEN
  RAISE EXCEPTION 'CMS_TENANT_SCOPE' USING ERRCODE='42501'; END IF;
 IF NOT p_read AND (p_kind IS NULL OR p_kind NOT IN ('user','api_key','service') OR p_actor IS NULL
  OR p_actor IS DISTINCT FROM nullif(current_setting('app.actor_id',true),'')::uuid
  OR p_kind IS DISTINCT FROM nullif(current_setting('app.actor_kind',true),'')) THEN
  RAISE EXCEPTION 'CMS_ACTOR_SCOPE' USING ERRCODE='42501'; END IF;
 IF NOT p_read AND current_setting('transaction_isolation')<>'read committed' THEN
  RAISE EXCEPTION 'CMS_READ_COMMITTED_REQUIRED' USING ERRCODE='25001'; END IF;
END $$;
CREATE FUNCTION app.cms_revision_immutable() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,app,pg_temp AS $$ BEGIN
 IF TG_TABLE_NAME='site_releases' THEN
  IF NOT OLD.sealed THEN IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW; END IF;
 END IF;
 RAISE EXCEPTION 'CMS_REVISION_IMMUTABLE' USING ERRCODE='55000'; END $$;
CREATE TRIGGER cms_revision_immutable BEFORE UPDATE OR DELETE ON app.page_revisions FOR EACH ROW EXECUTE FUNCTION app.cms_revision_immutable();
CREATE TRIGGER cms_release_immutable BEFORE UPDATE OR DELETE ON app.site_releases FOR EACH ROW EXECUTE FUNCTION app.cms_revision_immutable();
-- Invoker trigger sees real caller, unlike a SECURITY DEFINER trigger. Parent
-- row locks serialize raw legacy writers with managed publication/close.
CREATE FUNCTION app.cms_draft_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE managed boolean; closed boolean; t uuid; s uuid; d uuid;
BEGIN
 IF TG_TABLE_NAME='site_drafts' THEN
  IF TG_OP='DELETE' THEN
   IF OLD.is_cms AND current_user<>'app_publish_executor' THEN RAISE EXCEPTION 'CMS_CONTROLLED_WRITE_REQUIRED' USING ERRCODE='42501'; END IF;
   IF OLD.status<>'open' THEN RAISE EXCEPTION 'CMS_DRAFT_CLOSED' USING ERRCODE='55000'; END IF; RETURN OLD;
  END IF;
  IF TG_OP='INSERT' THEN
   IF NEW.is_cms AND current_user<>'app_publish_executor' THEN RAISE EXCEPTION 'CMS_CONTROLLED_WRITE_REQUIRED' USING ERRCODE='42501'; END IF;
   RETURN NEW;
  END IF;
  IF OLD.is_cms IS DISTINCT FROM NEW.is_cms OR ROW(OLD.tenant_id,OLD.site_id,OLD.id,OLD.base_release_id) IS DISTINCT FROM ROW(NEW.tenant_id,NEW.site_id,NEW.id,NEW.base_release_id) THEN
   RAISE EXCEPTION 'CMS_DRAFT_IDENTITY_IMMUTABLE' USING ERRCODE='55000'; END IF;
  IF OLD.status<>'open' THEN RAISE EXCEPTION 'CMS_DRAFT_CLOSED' USING ERRCODE='55000'; END IF;
  IF OLD.is_cms AND current_user<>'app_publish_executor' THEN RAISE EXCEPTION 'CMS_CONTROLLED_WRITE_REQUIRED' USING ERRCODE='42501'; END IF;
 ELSE
  IF TG_OP<>'INSERT' THEN
   SELECT is_cms,status<>'open' INTO managed,closed FROM app.site_drafts WHERE tenant_id=OLD.tenant_id AND site_id=OLD.site_id AND id=OLD.draft_id FOR SHARE;
   IF NOT FOUND THEN RAISE EXCEPTION 'CMS_DRAFT_NOT_FOUND' USING ERRCODE='23503'; END IF;
   IF closed THEN RAISE EXCEPTION 'CMS_DRAFT_CLOSED' USING ERRCODE='55000'; END IF;
   IF managed AND current_user<>'app_publish_executor' THEN RAISE EXCEPTION 'CMS_CONTROLLED_WRITE_REQUIRED' USING ERRCODE='42501'; END IF;
  END IF;
  IF TG_OP<>'DELETE' THEN
   SELECT is_cms,status<>'open' INTO managed,closed FROM app.site_drafts WHERE tenant_id=NEW.tenant_id AND site_id=NEW.site_id AND id=NEW.draft_id FOR SHARE;
   IF NOT FOUND THEN RAISE EXCEPTION 'CMS_DRAFT_NOT_FOUND' USING ERRCODE='23503'; END IF;
   IF closed THEN RAISE EXCEPTION 'CMS_DRAFT_CLOSED' USING ERRCODE='55000'; END IF;
   IF managed AND current_user<>'app_publish_executor' THEN RAISE EXCEPTION 'CMS_CONTROLLED_WRITE_REQUIRED' USING ERRCODE='42501'; END IF;
  END IF;
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW;
END $$;
CREATE TRIGGER cms_draft_guard BEFORE INSERT OR UPDATE OR DELETE ON app.site_drafts FOR EACH ROW EXECUTE FUNCTION app.cms_draft_guard();
CREATE TRIGGER cms_draft_page_guard BEFORE INSERT OR UPDATE OR DELETE ON app.draft_pages FOR EACH ROW EXECUTE FUNCTION app.cms_draft_guard();
CREATE TRIGGER cms_draft_media_guard BEFORE INSERT OR UPDATE OR DELETE ON app.draft_page_media FOR EACH ROW EXECUTE FUNCTION app.cms_draft_guard();

CREATE FUNCTION app.cms_validate_document(p_document jsonb) RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE b jsonb; BEGIN
 IF p_document IS NULL OR jsonb_typeof(p_document)<>'object' OR p_document - ARRAY['title','description','blocks'] <> '{}'::jsonb
  OR jsonb_typeof(p_document->'title') IS DISTINCT FROM 'string' OR length(p_document->>'title') NOT BETWEEN 1 AND 300
  OR (p_document ? 'description' AND (jsonb_typeof(p_document->'description')<>'string' OR length(p_document->>'description')>2000))
  OR jsonb_typeof(p_document->'blocks') IS DISTINCT FROM 'array' THEN RAISE EXCEPTION 'CMS_DOCUMENT_SHAPE' USING ERRCODE='22023'; END IF;
 IF jsonb_array_length(p_document->'blocks')>200 OR btrim(p_document->>'title')='' OR (p_document->>'title')~'[<>]' OR coalesce(p_document->>'description','')~'[<>]' THEN RAISE EXCEPTION 'CMS_DOCUMENT_BOUNDS' USING ERRCODE='22023'; END IF;
 FOR b IN SELECT value FROM jsonb_array_elements(p_document->'blocks') LOOP
  IF jsonb_typeof(b)<>'object' OR b - ARRAY['kind','text']<>'{}'::jsonb OR b->>'kind' NOT IN ('heading','paragraph')
   OR jsonb_typeof(b->'kind') IS DISTINCT FROM 'string' OR jsonb_typeof(b->'text') IS DISTINCT FROM 'string'
   OR btrim(b->>'text')='' OR length(b->>'text')>10000 OR b->>'text'~'[<>]' THEN RAISE EXCEPTION 'CMS_BLOCK_SHAPE' USING ERRCODE='22023'; END IF;
 END LOOP;
END $$;
CREATE FUNCTION app.cms_products(p_tenant uuid,p_site uuid,p_locale text) RETURNS jsonb
LANGUAGE sql STABLE SET search_path=pg_catalog,app,pg_temp AS $$
 SELECT coalesce(jsonb_agg(item ORDER BY slug),'[]'::jsonb) FROM (
  SELECT l.slug,jsonb_build_object('id',p.id,'slug',l.slug,'name',c.title,'description',coalesce(c.seo->>'description',''),
   'unit_amount',price.unit_amount::text,'currency',price.currency::text) item
  FROM app.product_listings l JOIN app.products p ON (p.tenant_id,p.id)=(l.tenant_id,l.product_id)
  JOIN app.product_content_revisions c ON (c.tenant_id,c.product_id,c.locale,c.revision)=(l.tenant_id,l.product_id,l.locale,l.published_content_revision)
  -- MVP display selection: smallest existing SKU UUID; qty1/all price priority
  -- DESC/list UUID ASC. Unpriceable representative SKU omits the product.
  JOIN LATERAL (SELECT id FROM app.product_variants v WHERE v.tenant_id=p_tenant AND v.product_id=p.id AND v.lifecycle_status='ready' ORDER BY id LIMIT 1) sku ON true
  JOIN LATERAL (SELECT pe.unit_amount,pl.currency FROM app.price_entries pe JOIN app.price_lists pl ON (pl.tenant_id,pl.id)=(pe.tenant_id,pe.price_list_id)
   WHERE pe.tenant_id=p_tenant AND pe.sku_id=sku.id AND pe.min_quantity=1 AND pl.audience='all' AND pl.status='active'
    AND pl.starts_at<=statement_timestamp() AND (pl.ends_at IS NULL OR statement_timestamp()<pl.ends_at)
   ORDER BY pl.priority DESC,pl.id ASC LIMIT 1) price ON true
  WHERE l.tenant_id=p_tenant AND l.site_id=p_site AND lower(l.locale)=lower(p_locale) AND l.status='published' AND p.lifecycle_status='ready'
  ORDER BY l.slug LIMIT 501
 ) eligible
$$;
CREATE FUNCTION app.cms_read_draft(p_tenant uuid,p_site uuid,p_draft uuid,p_expected bigint DEFAULT NULL) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE d app.site_drafts; pages jsonb; products jsonb; epoch bigint;
BEGIN
 PERFORM app.cms_context(p_tenant,NULL,NULL,true);
 IF current_setting('transaction_read_only')='on' THEN
  IF current_setting('transaction_isolation') NOT IN ('repeatable read','serializable') THEN RAISE EXCEPTION 'CMS_READ_SNAPSHOT_REQUIRED' USING ERRCODE='25001'; END IF;
  SELECT * INTO d FROM app.site_drafts WHERE tenant_id=p_tenant AND site_id=p_site AND id=p_draft AND is_cms;
 ELSE
  -- All write transactions lock site before draft. A pre-publish read must
  -- acquire the same exclusive site lock rather than upgrading later.
  PERFORM FROM app.sites WHERE tenant_id=p_tenant AND id=p_site FOR UPDATE;
  SELECT * INTO d FROM app.site_drafts WHERE tenant_id=p_tenant AND site_id=p_site AND id=p_draft AND is_cms FOR SHARE;
 END IF;
 IF NOT FOUND THEN RAISE EXCEPTION 'CMS_DRAFT_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF p_expected IS NOT NULL AND p_expected<>d.revision THEN RAISE EXCEPTION 'CMS_DRAFT_STALE' USING ERRCODE='40001'; END IF;
 SELECT publication_revision INTO epoch FROM app.sites WHERE tenant_id=p_tenant AND id=p_site;
 SELECT coalesce(jsonb_agg(jsonb_build_object('id',dp.page_id,'page_key',p.page_key,'locale',dp.locale,'path',dp.path,'document',dp.document,
  'media_ids',(SELECT coalesce(jsonb_agg(m.media_id ORDER BY m.media_id),'[]') FROM app.draft_page_media m WHERE (m.tenant_id,m.site_id,m.draft_id,m.page_id,m.locale)=(dp.tenant_id,dp.site_id,dp.draft_id,dp.page_id,dp.locale))) ORDER BY dp.path),'[]') INTO pages
  FROM app.draft_pages dp JOIN app.pages p ON (p.tenant_id,p.site_id,p.id)=(dp.tenant_id,dp.site_id,dp.page_id)
  WHERE dp.tenant_id=p_tenant AND dp.site_id=p_site AND dp.draft_id=p_draft;
 products:=app.cms_products(p_tenant,p_site,d.locale);
 RETURN jsonb_build_object('draft_id',d.id,'site_id',p_site,'revision',d.revision,'base_release_id',d.base_release_id,'publication_revision',epoch,
  'status',d.status,'locale',d.locale,'title',d.title,'description',d.description,'navigation',d.navigation,'pages',pages,'products',products);
END $$;
CREATE FUNCTION app.cms_create_draft(p_tenant uuid,p_site uuid,p_kind text,p_actor uuid,p_locale text,p_title text,p_description text) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE s app.sites; d uuid:=uuidv7(); route record; cloned_page uuid; BEGIN
 PERFORM app.cms_context(p_tenant,p_kind,p_actor);
 SELECT * INTO s FROM app.sites WHERE tenant_id=p_tenant AND id=p_site FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'CMS_SITE_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF p_locale IS NULL OR lower(p_locale)<>lower(s.default_locale) OR p_title IS NULL OR length(p_title) NOT BETWEEN 1 AND 300 OR btrim(p_title)='' OR p_title~'[<>]'
  OR p_description IS NULL OR length(p_description)>2000 OR p_description~'[<>]' THEN RAISE EXCEPTION 'CMS_METADATA_SHAPE' USING ERRCODE='22023'; END IF;
 INSERT INTO app.site_drafts(tenant_id,site_id,id,base_release_id,is_cms,locale,title,description,navigation)
 VALUES(p_tenant,p_site,d,s.published_release_id,true,p_locale,p_title,p_description,
  coalesce((SELECT manifest->'navigation' FROM app.site_releases WHERE tenant_id=p_tenant AND site_id=p_site AND id=s.published_release_id),'[]'));
 -- Draft pages have one path each. Preserve legacy release aliases as distinct
 -- editable pages instead of silently discarding routes/navigation targets.
 FOR route IN SELECT rr.page_id,rr.locale,rr.path,pr.document FROM app.site_release_routes rr
  JOIN app.page_revisions pr ON (pr.tenant_id,pr.site_id,pr.page_id,pr.locale,pr.revision)=(rr.tenant_id,rr.site_id,rr.page_id,rr.locale,rr.page_revision)
  WHERE rr.tenant_id=p_tenant AND rr.site_id=p_site AND rr.release_id=s.published_release_id AND lower(rr.locale)=lower(p_locale)
  ORDER BY rr.page_id,rr.locale,length(rr.path),rr.path LOOP
  cloned_page:=route.page_id;
  IF EXISTS(SELECT FROM app.draft_pages WHERE tenant_id=p_tenant AND site_id=p_site AND draft_id=d AND page_id=cloned_page AND locale=p_locale) THEN
   cloned_page:=uuidv7();
   INSERT INTO app.pages(tenant_id,site_id,id,page_key) VALUES(p_tenant,p_site,cloned_page,'cms-alias-'||cloned_page::text);
  END IF;
  INSERT INTO app.draft_pages(tenant_id,site_id,draft_id,page_id,locale,path,document)
   VALUES(p_tenant,p_site,d,cloned_page,p_locale,route.path,route.document);
 END LOOP;
 -- Each cloned page gets only its own sealed revision's pins (never the
 -- page x release cross product, which grows N*M and breaks the 1 MiB read).
 INSERT INTO app.draft_page_media SELECT p_tenant,p_site,d,dp.page_id,dp.locale,rm.media_id FROM app.draft_pages dp
 JOIN app.site_release_media rm ON rm.tenant_id=p_tenant AND rm.site_id=p_site AND rm.release_id=s.published_release_id WHERE dp.tenant_id=p_tenant AND dp.site_id=p_site AND dp.draft_id=d;
 INSERT INTO app.audit_events(tenant_id,actor_kind,actor_id,operation,resource_type,resource_id,payload)
 VALUES(p_tenant,p_kind,p_actor,'cms.create_draft','site_draft',d,jsonb_build_object('site_id',p_site,'base_release_id',s.published_release_id));
 RETURN app.cms_read_draft(p_tenant,p_site,d,NULL);
END $$;
CREATE FUNCTION app.cms_apply(p_tenant uuid,p_site uuid,p_draft uuid,p_kind text,p_actor uuid,p_expected bigint,p_operation text,p_payload jsonb) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE d app.site_drafts; actions jsonb; action jsonb; op text; body jsonb; p uuid; m jsonb; nav jsonb; item jsonb; response jsonb;
BEGIN
 PERFORM app.cms_context(p_tenant,p_kind,p_actor);
 PERFORM FROM app.sites WHERE tenant_id=p_tenant AND id=p_site FOR UPDATE;
 SELECT * INTO d FROM app.site_drafts WHERE tenant_id=p_tenant AND site_id=p_site AND id=p_draft AND is_cms FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'CMS_DRAFT_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF d.status<>'open' THEN RAISE EXCEPTION 'CMS_DRAFT_CLOSED' USING ERRCODE='55000'; END IF;
 IF p_expected IS NULL OR p_expected<>d.revision THEN RAISE EXCEPTION 'CMS_DRAFT_STALE' USING ERRCODE='40001'; END IF;
 IF p_payload IS NULL OR jsonb_typeof(p_payload)<>'object' THEN RAISE EXCEPTION 'CMS_OPERATION_SHAPE' USING ERRCODE='22023'; END IF;
 IF p_operation='apply_operations' THEN
  IF p_payload-ARRAY['operations']<>'{}' OR jsonb_typeof(p_payload->'operations') IS DISTINCT FROM 'array' THEN RAISE EXCEPTION 'CMS_BATCH_SHAPE' USING ERRCODE='22023'; END IF;
  actions:=p_payload->'operations';
  IF jsonb_array_length(actions) NOT BETWEEN 1 AND 100 THEN RAISE EXCEPTION 'CMS_BATCH_BOUNDS' USING ERRCODE='22023'; END IF;
 ELSE actions:=jsonb_build_array(jsonb_build_object('operation',p_operation,'payload',p_payload)); END IF;
 FOR action IN SELECT value FROM jsonb_array_elements(actions) LOOP
  IF jsonb_typeof(action)<>'object' OR action-ARRAY['operation','payload']<>'{}' OR jsonb_typeof(action->'operation') IS DISTINCT FROM 'string' OR jsonb_typeof(action->'payload') IS DISTINCT FROM 'object' THEN RAISE EXCEPTION 'CMS_OPERATION_SHAPE' USING ERRCODE='22023'; END IF;
  op:=action->>'operation'; body:=action->'payload';
  CASE op
  WHEN 'save_page','update_page' THEN
   IF body-ARRAY['id','page_key','locale','path','document','media_ids']<>'{}' OR jsonb_typeof(body->'page_key') IS DISTINCT FROM 'string'
    OR length(body->>'page_key') NOT BETWEEN 1 AND 128 OR btrim(body->>'page_key')='' OR body->>'page_key'~'[<>]' OR body->>'locale' IS DISTINCT FROM d.locale
    OR jsonb_typeof(body->'path') IS DISTINCT FROM 'string' OR (body->>'path') !~ '^/[a-zA-Z0-9_-]+(/[a-zA-Z0-9_-]+)*$' AND body->>'path'<>'/'
    OR length(body->>'path')>512 OR body->>'path' IN ('/products','/404','/502') OR body->>'path' LIKE '/products/%'
    OR jsonb_typeof(body->'media_ids') IS DISTINCT FROM 'array' THEN RAISE EXCEPTION 'CMS_PAGE_SHAPE' USING ERRCODE='22023'; END IF;
   PERFORM app.cms_validate_document(body->'document');
   IF jsonb_array_length(body->'media_ids')>100 OR (SELECT count(DISTINCT value) FROM jsonb_array_elements(body->'media_ids'))<>jsonb_array_length(body->'media_ids') THEN RAISE EXCEPTION 'CMS_MEDIA_BOUNDS' USING ERRCODE='22023'; END IF;
   p:=coalesce((body->>'id')::uuid,uuidv7());
   IF op='update_page' AND (body->>'id' IS NULL OR NOT EXISTS(SELECT FROM app.draft_pages WHERE tenant_id=p_tenant AND site_id=p_site AND draft_id=p_draft AND page_id=p AND locale=d.locale)) THEN
    RAISE EXCEPTION 'CMS_PAGE_NOT_FOUND' USING ERRCODE='P0002';
   END IF;
   IF body->>'id' IS NULL THEN
    -- A page_key is a site-wide identity: reattach it when its page left this
    -- draft (deleted/unpublished); a key already in the draft stays a conflict.
    SELECT id INTO p FROM app.pages WHERE tenant_id=p_tenant AND site_id=p_site AND page_key=body->>'page_key';
    IF NOT FOUND THEN
     p:=uuidv7();
     INSERT INTO app.pages(tenant_id,site_id,id,page_key) VALUES(p_tenant,p_site,p,body->>'page_key');
    ELSIF EXISTS(SELECT FROM app.draft_pages WHERE tenant_id=p_tenant AND site_id=p_site AND draft_id=p_draft AND page_id=p) THEN
     RAISE EXCEPTION 'CMS_PAGE_KEY_EXISTS' USING ERRCODE='23505';
    END IF;
   ELSIF NOT EXISTS(SELECT FROM app.pages WHERE tenant_id=p_tenant AND site_id=p_site AND id=p AND page_key=body->>'page_key') THEN RAISE EXCEPTION 'CMS_PAGE_NOT_FOUND' USING ERRCODE='P0002'; END IF;
   INSERT INTO app.draft_pages(tenant_id,site_id,draft_id,page_id,locale,path,document)
   VALUES(p_tenant,p_site,p_draft,p,d.locale,body->>'path',body->'document')
   ON CONFLICT(tenant_id,site_id,draft_id,page_id,locale) DO UPDATE SET path=EXCLUDED.path,document=EXCLUDED.document;
   DELETE FROM app.draft_page_media WHERE tenant_id=p_tenant AND site_id=p_site AND draft_id=p_draft AND page_id=p AND locale=d.locale;
   FOR m IN SELECT value FROM jsonb_array_elements(body->'media_ids') LOOP
    IF jsonb_typeof(m)<>'string' THEN RAISE EXCEPTION 'CMS_MEDIA_SHAPE' USING ERRCODE='22023'; END IF;
    INSERT INTO app.draft_page_media VALUES(p_tenant,p_site,p_draft,p,d.locale,(m#>>'{}')::uuid) ON CONFLICT DO NOTHING;
   END LOOP;
  WHEN 'delete_page' THEN
   IF body-ARRAY['id','locale']<>'{}' OR body->>'id' IS NULL OR body->>'locale' IS DISTINCT FROM d.locale THEN RAISE EXCEPTION 'CMS_PAGE_SHAPE' USING ERRCODE='22023'; END IF;
   p:=(body->>'id')::uuid;
   DELETE FROM app.draft_page_media WHERE tenant_id=p_tenant AND site_id=p_site AND draft_id=p_draft AND page_id=p AND locale=d.locale;
   DELETE FROM app.draft_pages WHERE tenant_id=p_tenant AND site_id=p_site AND draft_id=p_draft AND page_id=p AND locale=d.locale;
   IF NOT FOUND THEN RAISE EXCEPTION 'CMS_PAGE_NOT_FOUND' USING ERRCODE='P0002'; END IF;
  WHEN 'set_navigation' THEN
   IF body-ARRAY['navigation']<>'{}' OR jsonb_typeof(body->'navigation') IS DISTINCT FROM 'array' THEN RAISE EXCEPTION 'CMS_NAVIGATION_SHAPE' USING ERRCODE='22023'; END IF;
   nav:=body->'navigation';
   IF jsonb_array_length(nav)>50 THEN RAISE EXCEPTION 'CMS_NAVIGATION_BOUNDS' USING ERRCODE='22023'; END IF;
   FOR item IN SELECT value FROM jsonb_array_elements(nav) LOOP
    IF jsonb_typeof(item)<>'object' OR item-ARRAY['label','href']<>'{}' OR jsonb_typeof(item->'label') IS DISTINCT FROM 'string'
     OR length(item->>'label') NOT BETWEEN 1 AND 200 OR btrim(item->>'label')='' OR item->>'label'~'[<>]' OR jsonb_typeof(item->'href') IS DISTINCT FROM 'string'
     OR NOT ((item->>'href')='/' OR (item->>'href')~'^/[a-zA-Z0-9_-]+(/[a-zA-Z0-9_-]+)*$') OR length(item->>'href')>512 THEN RAISE EXCEPTION 'CMS_NAVIGATION_SHAPE' USING ERRCODE='22023'; END IF;
   END LOOP;
   UPDATE app.site_drafts SET navigation=nav WHERE tenant_id=p_tenant AND site_id=p_site AND id=p_draft;
  WHEN 'set_metadata' THEN
   IF body-ARRAY['locale','title','description']<>'{}' OR body->>'locale' IS DISTINCT FROM d.locale
    OR jsonb_typeof(body->'title') IS DISTINCT FROM 'string' OR length(body->>'title') NOT BETWEEN 1 AND 300 OR btrim(body->>'title')='' OR body->>'title'~'[<>]'
    OR jsonb_typeof(body->'description') IS DISTINCT FROM 'string' OR length(body->>'description')>2000 OR body->>'description'~'[<>]' THEN RAISE EXCEPTION 'CMS_METADATA_SHAPE' USING ERRCODE='22023'; END IF;
   UPDATE app.site_drafts SET title=body->>'title',description=body->>'description' WHERE tenant_id=p_tenant AND site_id=p_site AND id=p_draft;
  ELSE RAISE EXCEPTION 'CMS_OPERATION_UNKNOWN' USING ERRCODE='22023';
  END CASE;
 END LOOP;
 IF (SELECT count(*) FROM app.draft_pages WHERE tenant_id=p_tenant AND site_id=p_site AND draft_id=p_draft)>100 THEN RAISE EXCEPTION 'CMS_PAGE_BOUNDS' USING ERRCODE='22023'; END IF;
 IF EXISTS(SELECT FROM app.site_drafts sd CROSS JOIN LATERAL jsonb_array_elements(sd.navigation) i WHERE sd.tenant_id=p_tenant AND sd.site_id=p_site AND sd.id=p_draft AND i->>'href'<>'/products'
  AND NOT EXISTS(SELECT FROM app.draft_pages dp WHERE dp.tenant_id=p_tenant AND dp.site_id=p_site AND dp.draft_id=p_draft AND dp.path=i->>'href')
  AND NOT EXISTS(SELECT FROM jsonb_array_elements(app.cms_products(p_tenant,p_site,d.locale)) product WHERE '/products/'||(product->>'slug')=i->>'href')) THEN RAISE EXCEPTION 'CMS_NAVIGATION_TARGET' USING ERRCODE='22023'; END IF;
 UPDATE app.site_drafts SET revision=revision+1 WHERE tenant_id=p_tenant AND site_id=p_site AND id=p_draft;
 INSERT INTO app.draft_operations(tenant_id,site_id,draft_id,operation_id,actor_id,before_revision,after_revision,operation)
 VALUES(p_tenant,p_site,p_draft,uuidv7(),p_actor,d.revision,d.revision+1,jsonb_build_object('operation',p_operation,'payload',p_payload));
 INSERT INTO app.audit_events(tenant_id,actor_kind,actor_id,operation,resource_type,resource_id,before_revision,after_revision,payload)
 VALUES(p_tenant,p_kind,p_actor,'cms.apply','site_draft',p_draft,d.revision,d.revision+1,jsonb_build_object('site_id',p_site,'operation',p_operation));
 response:=app.cms_read_draft(p_tenant,p_site,p_draft,NULL);
 IF octet_length(convert_to(response::text,'UTF8'))>1048000 THEN RAISE EXCEPTION 'CMS_PROJECTION_BOUNDS' USING ERRCODE='22023'; END IF;
 RETURN response;
END $$;
CREATE FUNCTION app.cms_publish(p_tenant uuid,p_site uuid,p_draft uuid,p_kind text,p_actor uuid,p_expected bigint,p_expected_release uuid,p_epoch bigint,p_renderer text) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE s app.sites; d app.site_drafts; r uuid:=uuidv7(); no bigint; manifest jsonb; products jsonb; snapshot_hash bytea; product_item jsonb; page record; revision bigint; result jsonb; BEGIN
 PERFORM app.cms_context(p_tenant,p_kind,p_actor);
 SELECT * INTO s FROM app.sites WHERE tenant_id=p_tenant AND id=p_site FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'CMS_SITE_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF p_epoch IS NULL OR s.publication_revision<>p_epoch OR s.published_release_id IS DISTINCT FROM p_expected_release THEN RAISE EXCEPTION 'CMS_PUBLICATION_STALE' USING ERRCODE='40001'; END IF;
 SELECT * INTO d FROM app.site_drafts WHERE tenant_id=p_tenant AND site_id=p_site AND id=p_draft AND is_cms FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'CMS_DRAFT_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF d.status<>'open' THEN RAISE EXCEPTION 'CMS_DRAFT_CLOSED' USING ERRCODE='55000'; END IF;
 IF p_expected IS NULL OR p_expected<>d.revision OR d.base_release_id IS DISTINCT FROM p_expected_release THEN RAISE EXCEPTION 'CMS_DRAFT_STALE' USING ERRCODE='40001'; END IF;
 IF p_renderer IS NULL OR length(p_renderer) NOT BETWEEN 1 AND 300 OR lower(d.locale)<>lower(s.default_locale) THEN RAISE EXCEPTION 'CMS_PUBLISH_ARGUMENT' USING ERRCODE='22023'; END IF;
 IF NOT EXISTS(SELECT FROM app.draft_pages WHERE tenant_id=p_tenant AND site_id=p_site AND draft_id=p_draft AND path='/' AND locale=d.locale) THEN RAISE EXCEPTION 'CMS_HOME_REQUIRED' USING ERRCODE='22023'; END IF;
 products:=app.cms_products(p_tenant,p_site,d.locale);
 IF jsonb_array_length(products)>500 THEN RAISE EXCEPTION 'CMS_PRODUCT_BOUNDS' USING ERRCODE='22023'; END IF;
 FOR product_item IN SELECT value FROM jsonb_array_elements(products) LOOP
  IF length(product_item->>'name')>300 OR btrim(product_item->>'name')='' OR length(product_item->>'description')>10000
   OR (product_item->>'slug') !~ '^[a-zA-Z0-9_-]+$' OR length(product_item->>'slug')>502
   OR (product_item->>'unit_amount') !~ '^(0|[1-9][0-9]{0,37})$' OR (product_item->>'currency') !~ '^[A-Z]{3}$' THEN RAISE EXCEPTION 'CMS_CATALOG_PROJECTION_INVALID' USING ERRCODE='22023'; END IF;
 END LOOP;
 IF (SELECT count(*) FROM app.draft_pages WHERE tenant_id=p_tenant AND site_id=p_site AND draft_id=p_draft)>100 THEN RAISE EXCEPTION 'CMS_PAGE_BOUNDS' USING ERRCODE='22023'; END IF;
 -- Validate against this exact product snapshot: an unpriceable or unpublished
 -- target cannot slip through between service validation and transaction seal.
 IF EXISTS(SELECT FROM jsonb_array_elements(d.navigation) item WHERE item->>'href'<>'/products'
  AND NOT EXISTS(SELECT FROM app.draft_pages dp WHERE dp.tenant_id=p_tenant AND dp.site_id=p_site AND dp.draft_id=p_draft AND dp.path=item->>'href')
  AND NOT EXISTS(SELECT FROM jsonb_array_elements(products) product WHERE '/products/'||(product->>'slug')=item->>'href')) THEN RAISE EXCEPTION 'CMS_NAVIGATION_TARGET' USING ERRCODE='22023'; END IF;
 -- Keep PR35 v1 projection exact: no editable catalog IDs, prices or raw HTML.
 manifest:=jsonb_build_object('version',1,'locale',d.locale,'title',d.title,'description',d.description,'navigation',d.navigation,'products',products);
 result:=app.cms_read_draft(p_tenant,p_site,p_draft,NULL);
 IF octet_length(convert_to(result::text,'UTF8'))>1048000 THEN RAISE EXCEPTION 'CMS_PROJECTION_BOUNDS' USING ERRCODE='22023'; END IF;
 SELECT sha256(convert_to(jsonb_build_object('manifest',manifest,
  'routes',(SELECT jsonb_agg(jsonb_build_object('locale',locale,'path',path,'document',document) ORDER BY locale,path) FROM app.draft_pages WHERE tenant_id=p_tenant AND site_id=p_site AND draft_id=p_draft),
  'media_ids',(SELECT coalesce(jsonb_agg(media_id ORDER BY media_id),'[]') FROM (SELECT DISTINCT media_id FROM app.draft_page_media WHERE tenant_id=p_tenant AND site_id=p_site AND draft_id=p_draft) pins))::text,'UTF8')) INTO snapshot_hash;
 SELECT coalesce(max(release_no),0)+1 INTO no FROM app.site_releases WHERE tenant_id=p_tenant AND site_id=p_site;
 INSERT INTO app.site_releases(tenant_id,site_id,id,release_no,renderer_build_id,manifest,manifest_hash) VALUES(p_tenant,p_site,r,no,p_renderer,manifest,snapshot_hash);
 FOR page IN SELECT * FROM app.draft_pages WHERE tenant_id=p_tenant AND site_id=p_site AND draft_id=p_draft ORDER BY page_id,locale LOOP
  PERFORM app.cms_validate_document(page.document);
  SELECT coalesce(max(pr.revision),0)+1 INTO revision FROM app.page_revisions pr WHERE pr.tenant_id=p_tenant AND pr.site_id=p_site AND pr.page_id=page.page_id AND pr.locale=page.locale;
  INSERT INTO app.page_revisions(tenant_id,site_id,page_id,locale,revision,document,media_ids) VALUES(p_tenant,p_site,page.page_id,page.locale,revision,page.document,
   (SELECT coalesce(array_agg(media_id ORDER BY media_id),'{}') FROM app.draft_page_media WHERE tenant_id=p_tenant AND site_id=p_site AND draft_id=p_draft AND page_id=page.page_id AND locale=page.locale));
  INSERT INTO app.site_release_routes VALUES(p_tenant,p_site,r,page.locale,page.path,page.page_id,revision);
 END LOOP;
 INSERT INTO app.site_release_media SELECT DISTINCT p_tenant,p_site,r,media_id FROM app.draft_page_media WHERE tenant_id=p_tenant AND site_id=p_site AND draft_id=p_draft;
 PERFORM app.publish_release(p_tenant,p_site,r,p_expected_release);
 UPDATE app.site_drafts SET status='published' WHERE tenant_id=p_tenant AND site_id=p_site AND id=p_draft;
 INSERT INTO app.audit_events(tenant_id,actor_kind,actor_id,operation,resource_type,resource_id,before_revision,after_revision,payload)
 VALUES(p_tenant,p_kind,p_actor,'cms.publish','site',p_site,s.publication_revision,s.publication_revision+1,jsonb_build_object('draft_id',p_draft,'from_release_id',s.published_release_id,'to_release_id',r));
 RETURN jsonb_build_object('release_id',r,'release_no',no,'publication_revision',s.publication_revision+1,'restored_from',NULL,'manifest_hash',encode(snapshot_hash,'hex'),'created_at',(SELECT created_at FROM app.site_releases WHERE tenant_id=p_tenant AND site_id=p_site AND id=r));
END $$;
CREATE FUNCTION app.cms_restore(p_tenant uuid,p_site uuid,p_kind text,p_actor uuid,p_expected_release uuid,p_epoch bigint,p_target uuid) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE s app.sites; r app.site_releases; item jsonb; page record; route_count integer; projection jsonb; BEGIN
 PERFORM app.cms_context(p_tenant,p_kind,p_actor);
 SELECT * INTO s FROM app.sites WHERE tenant_id=p_tenant AND id=p_site FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'CMS_SITE_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF p_epoch IS NULL OR s.publication_revision<>p_epoch OR s.published_release_id IS DISTINCT FROM p_expected_release THEN RAISE EXCEPTION 'CMS_PUBLICATION_STALE' USING ERRCODE='40001'; END IF;
 SELECT * INTO r FROM app.site_releases WHERE tenant_id=p_tenant AND site_id=p_site AND id=p_target AND sealed FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'CMS_RELEASE_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 IF r.manifest->>'version' IS DISTINCT FROM '1' OR lower(r.manifest->>'locale') IS DISTINCT FROM lower(s.default_locale)
  OR jsonb_typeof(r.manifest)<>'object' OR r.manifest - ARRAY['version','locale','title','description','navigation','products']<>'{}'::jsonb
  OR jsonb_typeof(r.manifest->'version') IS DISTINCT FROM 'number' OR jsonb_typeof(r.manifest->'locale') IS DISTINCT FROM 'string'
  OR jsonb_typeof(r.manifest->'title') IS DISTINCT FROM 'string' OR length(r.manifest->>'title') NOT BETWEEN 1 AND 300 OR btrim(r.manifest->>'title')=''
  OR jsonb_typeof(r.manifest->'description') IS DISTINCT FROM 'string' OR length(r.manifest->>'description')>2000
  OR jsonb_typeof(r.manifest->'navigation') IS DISTINCT FROM 'array' OR jsonb_typeof(r.manifest->'products') IS DISTINCT FROM 'array'
  THEN RAISE EXCEPTION 'CMS_RELEASE_INCOMPATIBLE' USING ERRCODE='22023'; END IF;
 IF jsonb_array_length(r.manifest->'navigation')>50 OR jsonb_array_length(r.manifest->'products')>500 THEN RAISE EXCEPTION 'CMS_RELEASE_BOUNDS' USING ERRCODE='22023'; END IF;
 FOR item IN SELECT value FROM jsonb_array_elements(r.manifest->'products') LOOP
  IF jsonb_typeof(item)<>'object' OR item - ARRAY['id','slug','name','description','unit_amount','currency']<>'{}'::jsonb
   OR EXISTS(SELECT FROM unnest(ARRAY['id','slug','name','description','unit_amount','currency']) key WHERE jsonb_typeof(item->key) IS DISTINCT FROM 'string')
   OR (item->>'id') !~ '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
   OR (item->>'slug') !~ '^[a-zA-Z0-9_-]+$' OR length(item->>'slug')>502 OR length(item->>'name') NOT BETWEEN 1 AND 300 OR btrim(item->>'name')=''
   OR length(item->>'description')>10000 OR (item->>'unit_amount') !~ '^(0|[1-9][0-9]{0,37})$' OR (item->>'currency') !~ '^[A-Z]{3}$' THEN RAISE EXCEPTION 'CMS_RELEASE_PRODUCTS' USING ERRCODE='22023'; END IF;
 END LOOP;
 IF EXISTS(SELECT FROM jsonb_array_elements(r.manifest->'products') p GROUP BY p->>'id' HAVING count(*)>1)
  OR EXISTS(SELECT FROM jsonb_array_elements(r.manifest->'products') p GROUP BY p->>'slug' HAVING count(*)>1) THEN RAISE EXCEPTION 'CMS_RELEASE_PRODUCTS' USING ERRCODE='22023'; END IF;
 SELECT count(*) INTO route_count FROM app.site_release_routes WHERE tenant_id=p_tenant AND site_id=p_site AND release_id=p_target AND lower(locale)=lower(s.default_locale);
 IF route_count NOT BETWEEN 1 AND 100 OR NOT EXISTS(SELECT FROM app.site_release_routes WHERE tenant_id=p_tenant AND site_id=p_site AND release_id=p_target AND lower(locale)=lower(s.default_locale) AND path='/') THEN RAISE EXCEPTION 'CMS_RELEASE_ROUTES' USING ERRCODE='22023'; END IF;
 FOR page IN SELECT rr.path,pr.document FROM app.site_release_routes rr JOIN app.page_revisions pr
  ON (pr.tenant_id,pr.site_id,pr.page_id,pr.locale,pr.revision)=(rr.tenant_id,rr.site_id,rr.page_id,rr.locale,rr.page_revision)
  WHERE rr.tenant_id=p_tenant AND rr.site_id=p_site AND rr.release_id=p_target AND lower(rr.locale)=lower(s.default_locale) LOOP
  IF length(page.path)>512 OR page.path !~ '^/([a-zA-Z0-9_-]+(/[a-zA-Z0-9_-]+)*)?$' OR page.path IN ('/404','/502','/products') OR page.path LIKE '/products/%' THEN RAISE EXCEPTION 'CMS_RELEASE_ROUTES' USING ERRCODE='22023'; END IF;
  PERFORM app.cms_validate_document(page.document);
 END LOOP;
 FOR item IN SELECT value FROM jsonb_array_elements(r.manifest->'navigation') LOOP
  IF jsonb_typeof(item)<>'object' OR item - ARRAY['label','href']<>'{}'::jsonb OR jsonb_typeof(item->'label') IS DISTINCT FROM 'string'
   OR length(item->>'label') NOT BETWEEN 1 AND 200 OR btrim(item->>'label')='' OR jsonb_typeof(item->'href') IS DISTINCT FROM 'string'
   OR length(item->>'href')>512 OR (item->>'href') !~ '^/([a-zA-Z0-9_-]+(/[a-zA-Z0-9_-]+)*)?$'
   OR (item->>'href'<>'/products' AND NOT EXISTS(SELECT FROM app.site_release_routes rr WHERE rr.tenant_id=p_tenant AND rr.site_id=p_site AND rr.release_id=p_target AND lower(rr.locale)=lower(s.default_locale) AND rr.path=item->>'href')
    AND NOT EXISTS(SELECT FROM jsonb_array_elements(r.manifest->'products') product WHERE '/products/'||(product->>'slug')=item->>'href')) THEN RAISE EXCEPTION 'CMS_RELEASE_NAVIGATION' USING ERRCODE='22023'; END IF;
 END LOOP;
 SELECT jsonb_build_object('manifest',r.manifest,'pages',jsonb_agg(jsonb_build_object('path',rr.path,'document',pr.document))) INTO projection
  FROM app.site_release_routes rr JOIN app.page_revisions pr ON (pr.tenant_id,pr.site_id,pr.page_id,pr.locale,pr.revision)=(rr.tenant_id,rr.site_id,rr.page_id,rr.locale,rr.page_revision)
  WHERE rr.tenant_id=p_tenant AND rr.site_id=p_site AND rr.release_id=p_target AND lower(rr.locale)=lower(s.default_locale);
 IF octet_length(convert_to(projection::text,'UTF8'))>1040000 THEN RAISE EXCEPTION 'CMS_RELEASE_BOUNDS' USING ERRCODE='22023'; END IF;
 PERFORM app.publish_release(p_tenant,p_site,p_target,p_expected_release);
 INSERT INTO app.audit_events(tenant_id,actor_kind,actor_id,operation,resource_type,resource_id,before_revision,after_revision,payload)
 VALUES(p_tenant,p_kind,p_actor,'cms.restore','site',p_site,s.publication_revision,s.publication_revision+1,jsonb_build_object('from_release_id',s.published_release_id,'to_release_id',r.id));
 RETURN jsonb_build_object('release_id',r.id,'release_no',r.release_no,'publication_revision',s.publication_revision+1,'restored_from',r.restored_from,'manifest_hash',encode(r.manifest_hash,'hex'),'created_at',r.created_at);
END $$;
CREATE FUNCTION app.cms_release_history(p_tenant uuid,p_site uuid) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,app,pg_temp AS $$
DECLARE result jsonb; BEGIN
 PERFORM app.cms_context(p_tenant,NULL,NULL,true);
 IF NOT EXISTS(SELECT FROM app.sites WHERE tenant_id=p_tenant AND id=p_site) THEN RAISE EXCEPTION 'CMS_SITE_NOT_FOUND' USING ERRCODE='P0002'; END IF;
 SELECT coalesce(jsonb_agg(jsonb_build_object('release_id',r.id,'release_no',r.release_no,'created_at',r.created_at,'publication_revision',s.publication_revision,'restored_from',r.restored_from,'manifest_hash',encode(r.manifest_hash,'hex')) ORDER BY r.release_no DESC),'[]') INTO result
 FROM (SELECT * FROM app.site_releases WHERE tenant_id=p_tenant AND site_id=p_site AND sealed ORDER BY release_no DESC LIMIT 100) r JOIN app.sites s ON (s.tenant_id,s.id)=(r.tenant_id,r.site_id);
 RETURN jsonb_build_object('site_id',p_site,'publication_revision',(SELECT publication_revision FROM app.sites WHERE tenant_id=p_tenant AND id=p_site),'releases',result);
END $$;
-- The function owner remains unreachable (010 role attributes/membership gate).
GRANT CREATE ON SCHEMA app TO app_publish_executor;
DO $own$ DECLARE f record; BEGIN
 FOR f IN SELECT oid::regprocedure signature,proname FROM pg_proc WHERE pronamespace='app'::regnamespace AND proname LIKE 'cms_%' LOOP
  EXECUTE format('ALTER FUNCTION %s OWNER TO app_publish_executor',f.signature);
  EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC',f.signature);
  IF f.proname IN ('cms_create_draft','cms_read_draft','cms_apply','cms_publish','cms_restore','cms_release_history') THEN EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO app_runtime',f.signature); END IF;
 END LOOP;
END $own$;
REVOKE CREATE ON SCHEMA app FROM app_publish_executor;
