-- stable-id: ds-277
-- depends-on: fix-266
-- Responsibility: only bump product_listings.version when a non-version column changed.
-- Dependencies: fix-266 trigger function/column; caller: migrations.
-- Invariants: same owner, SECURITY INVOKER, search_path, INSERT and trigger behavior.
-- Compatibility: identical signature, so the existing trigger keeps working. Rollback: reapply fix-266 body.
CREATE OR REPLACE FUNCTION app.catalog_listing_version() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER
SET search_path=pg_catalog,app,pg_temp AS $$
BEGIN
 IF TG_OP='INSERT' THEN NEW.version:=1;
 ELSIF (to_jsonb(NEW)-'version') IS DISTINCT FROM (to_jsonb(OLD)-'version') THEN
  NEW.version:=OLD.version+1; -- real change: advance CAS/ETag
 ELSE
  NEW.version:=OLD.version; -- no-op update: keep version so clients are not forced to 412
 END IF;
 RETURN NEW;
END $$;
