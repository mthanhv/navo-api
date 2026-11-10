-- Bind command replay to current access and preserve accounting handoff bytes.
ALTER TABLE idempotency_records ADD COLUMN authorization_hash bytea;
ALTER TABLE misa_exports ADD COLUMN file_content bytea;
ALTER TABLE misa_exports ADD COLUMN file_sha256 text;
ALTER TABLE misa_exports ADD COLUMN mapping_version text;
ALTER TABLE misa_exports ADD COLUMN correction_needed boolean NOT NULL DEFAULT false;

-- Upload identities survive an uncertain response without creating another attachment.
ALTER TABLE attachment_links ADD COLUMN client_id uuid;
CREATE UNIQUE INDEX attachment_links_client_id_key ON attachment_links(created_by, client_id) WHERE client_id IS NOT NULL;

CREATE FUNCTION preserve_misa_export() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.file_content IS NOT NULL AND
     (NEW.file_content IS DISTINCT FROM OLD.file_content OR NEW.file_sha256 IS DISTINCT FROM OLD.file_sha256 OR NEW.mapping_version IS DISTINCT FROM OLD.mapping_version) THEN
    RAISE EXCEPTION 'navo:export_immutable';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER misa_exports_immutable BEFORE UPDATE ON misa_exports FOR EACH ROW EXECUTE FUNCTION preserve_misa_export();
