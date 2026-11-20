-- Restore the pre-023 interfaces for older API images during rollout/rollback.
-- Keep these tables until every supported rollback image uses activities. Changes in
-- either representation are mirrored in the same transaction, including old INSERT
-- ... ON CONFLICT clients (which cannot write through a compatibility view).
CREATE TABLE notes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id uuid NOT NULL UNIQUE,
    customer_id uuid NOT NULL REFERENCES organizations(id),
    contract_id uuid REFERENCES contracts(id) ON DELETE SET NULL,
    body text NOT NULL CHECK (length(btrim(body)) BETWEEN 1 AND 5000),
    written_at timestamptz NOT NULL,
    author_id uuid NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notes_customer_idx ON notes(customer_id, written_at DESC);
CREATE TABLE tasks (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    title text NOT NULL CHECK (length(btrim(title)) BETWEEN 1 AND 300),
    due_on date,
    owner_id uuid NOT NULL REFERENCES users(id),
    customer_id uuid REFERENCES organizations(id),
    contract_id uuid REFERENCES contracts(id) ON DELETE SET NULL,
    note_id uuid REFERENCES notes(id) ON DELETE SET NULL,
    done_at timestamptz,
    created_by uuid NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX tasks_owner_open_idx ON tasks(owner_id, due_on) WHERE done_at IS NULL;
INSERT INTO notes
SELECT id, coalesce(client_id,id), organization_id, contract_id, summary, done_at, owner_id, created_at
FROM activities WHERE kind='note' AND status='done' AND organization_id IS NOT NULL AND NOT auto;
INSERT INTO tasks
SELECT a.id, left(a.summary,300), a.planned_on, a.owner_id, a.organization_id, a.contract_id, n.id, a.done_at, a.created_by, a.created_at
FROM activities a LEFT JOIN notes n ON n.id=a.parent_id WHERE a.kind='task' AND NOT a.auto;

CREATE FUNCTION sync_legacy_crm() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF pg_trigger_depth() > 1 THEN RETURN NULL; END IF;
    IF TG_OP='DELETE' THEN
        DELETE FROM activities WHERE id=OLD.id;
    ELSIF TG_TABLE_NAME='notes' THEN
        INSERT INTO activities(id,client_id,kind,status,organization_id,contract_id,summary,done_at,owner_id,created_by,created_at)
        VALUES(NEW.id,NEW.client_id,'note','done',NEW.customer_id,NEW.contract_id,NEW.body,NEW.written_at,NEW.author_id,NEW.author_id,NEW.created_at)
        ON CONFLICT(id) DO UPDATE SET client_id=EXCLUDED.client_id, organization_id=EXCLUDED.organization_id,
            contract_id=EXCLUDED.contract_id, summary=EXCLUDED.summary, done_at=EXCLUDED.done_at,
            owner_id=EXCLUDED.owner_id, updated_at=now();
    ELSE
        INSERT INTO activities(id,kind,status,organization_id,contract_id,parent_id,summary,planned_on,done_at,owner_id,created_by,created_at)
        VALUES(NEW.id,'task',CASE WHEN NEW.done_at IS NULL THEN 'planned' ELSE 'done' END,NEW.customer_id,NEW.contract_id,NEW.note_id,NEW.title,NEW.due_on,NEW.done_at,NEW.owner_id,NEW.created_by,NEW.created_at)
        ON CONFLICT(id) DO UPDATE SET status=EXCLUDED.status, organization_id=EXCLUDED.organization_id,
            contract_id=EXCLUDED.contract_id, parent_id=EXCLUDED.parent_id, summary=EXCLUDED.summary,
            planned_on=EXCLUDED.planned_on, done_at=EXCLUDED.done_at, owner_id=EXCLUDED.owner_id, updated_at=now();
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER notes_sync AFTER INSERT OR UPDATE OR DELETE ON notes FOR EACH ROW EXECUTE FUNCTION sync_legacy_crm();
CREATE TRIGGER tasks_sync AFTER INSERT OR UPDATE OR DELETE ON tasks FOR EACH ROW EXECUTE FUNCTION sync_legacy_crm();

CREATE FUNCTION sync_activity_legacy() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF pg_trigger_depth() > 1 THEN RETURN NULL; END IF;
    IF TG_OP='DELETE' THEN
        DELETE FROM tasks WHERE id=OLD.id;
        DELETE FROM notes WHERE id=OLD.id;
        RETURN NULL;
    END IF;
    IF NEW.kind='note' AND NEW.status='done' AND NEW.organization_id IS NOT NULL AND NOT NEW.auto THEN
        INSERT INTO notes(id,client_id,customer_id,contract_id,body,written_at,author_id,created_at)
        VALUES(NEW.id,coalesce(NEW.client_id,NEW.id),NEW.organization_id,NEW.contract_id,NEW.summary,NEW.done_at,NEW.owner_id,NEW.created_at)
        ON CONFLICT(id) DO UPDATE SET customer_id=EXCLUDED.customer_id, contract_id=EXCLUDED.contract_id,
            body=EXCLUDED.body, written_at=EXCLUDED.written_at, author_id=EXCLUDED.author_id;
    ELSIF NEW.kind='task' AND NOT NEW.auto THEN
        INSERT INTO tasks(id,title,due_on,owner_id,customer_id,contract_id,note_id,done_at,created_by,created_at)
        VALUES(NEW.id,left(NEW.summary,300),NEW.planned_on,NEW.owner_id,NEW.organization_id,NEW.contract_id,
            (SELECT id FROM notes WHERE id=NEW.parent_id),NEW.done_at,NEW.created_by,NEW.created_at)
        ON CONFLICT(id) DO UPDATE SET title=EXCLUDED.title, due_on=EXCLUDED.due_on, owner_id=EXCLUDED.owner_id,
            customer_id=EXCLUDED.customer_id, contract_id=EXCLUDED.contract_id, note_id=EXCLUDED.note_id, done_at=EXCLUDED.done_at;
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER activities_legacy_sync AFTER INSERT OR UPDATE OR DELETE ON activities FOR EACH ROW EXECUTE FUNCTION sync_activity_legacy();
