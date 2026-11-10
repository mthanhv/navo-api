ALTER TABLE mailing_recipients DROP CONSTRAINT mailing_recipients_status_check;
ALTER TABLE mailing_recipients ADD CONSTRAINT mailing_recipients_status_check CHECK (status IN ('queued','sending','sent','failed','unknown'));
ALTER TABLE mailing_products ADD COLUMN description text;
ALTER TABLE mailing_products ADD COLUMN origin text;
ALTER TABLE mailing_products ADD COLUMN snapshot_version int NOT NULL DEFAULT 0;

CREATE TABLE ai_extractions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 actor_id uuid NOT NULL REFERENCES users(id),
 source_hash text NOT NULL,
 model text NOT NULL,
 status text NOT NULL CHECK (status IN ('running','completed','failed')),
 result jsonb,
 created_at timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz,
 UNIQUE(actor_id,source_hash,model)
);
