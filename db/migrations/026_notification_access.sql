-- An outbox message must still belong to the authorization snapshot that rendered it.
-- Invalidate conservatively on any permission/assignment change; the next worker tick
-- rebuilds the daily messages and may replace only access-invalidated dedupe entries.
ALTER TABLE notification_settings ADD COLUMN authorization_version bigint NOT NULL DEFAULT 1;
ALTER TABLE notifications ADD COLUMN authorization_version bigint NOT NULL DEFAULT 0;
CREATE FUNCTION notification_access_version() RETURNS bigint LANGUAGE sql STABLE AS $$
 SELECT authorization_version FROM notification_settings
$$;
ALTER TABLE notifications ALTER COLUMN authorization_version SET DEFAULT notification_access_version();

CREATE FUNCTION invalidate_notification_access() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 UPDATE notification_settings SET authorization_version=authorization_version+1, last_digest_on=NULL;
 UPDATE notifications SET status='failed', error='access_changed' WHERE status='pending';
 RETURN NULL;
END $$;
CREATE TRIGGER notification_user_access AFTER UPDATE OF is_active,version ON users
 FOR EACH ROW WHEN (OLD.is_active IS DISTINCT FROM NEW.is_active OR OLD.version IS DISTINCT FROM NEW.version)
 EXECUTE FUNCTION invalidate_notification_access();
CREATE TRIGGER notification_role_access AFTER INSERT OR UPDATE OR DELETE ON user_roles
 FOR EACH STATEMENT EXECUTE FUNCTION invalidate_notification_access();
CREATE TRIGGER notification_grant_access AFTER INSERT OR UPDATE OR DELETE ON user_grants
 FOR EACH STATEMENT EXECUTE FUNCTION invalidate_notification_access();
CREATE TRIGGER notification_contract_owner AFTER UPDATE OF owner_id ON contracts
 FOR EACH ROW WHEN (OLD.owner_id IS DISTINCT FROM NEW.owner_id) EXECUTE FUNCTION invalidate_notification_access();
CREATE TRIGGER notification_quotation_owner AFTER UPDATE OF owner_id ON quotations
 FOR EACH ROW WHEN (OLD.owner_id IS DISTINCT FROM NEW.owner_id) EXECUTE FUNCTION invalidate_notification_access();
CREATE TRIGGER notification_rule_access AFTER UPDATE ON notification_rules
 FOR EACH STATEMENT EXECUTE FUNCTION invalidate_notification_access();
