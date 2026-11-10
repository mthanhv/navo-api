ALTER TABLE purchase_order_lines ADD COLUMN cancelled_quantity numeric(20,6) NOT NULL DEFAULT 0 CHECK (cancelled_quantity >= 0 AND cancelled_quantity <= quantity);
CREATE TABLE purchase_order_closures (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), purchase_order_id uuid NOT NULL REFERENCES purchase_orders(id),
 quantities jsonb NOT NULL, reason text NOT NULL CHECK(length(btrim(reason)) BETWEEN 1 AND 500),
 created_by uuid NOT NULL REFERENCES users(id), created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER purchase_order_closures_immutable BEFORE UPDATE OR DELETE ON purchase_order_closures FOR EACH ROW EXECUTE FUNCTION forbid_history_change();
CREATE TABLE handovers (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), from_user uuid NOT NULL REFERENCES users(id), to_user uuid NOT NULL REFERENCES users(id),
 reason text NOT NULL CHECK(length(btrim(reason)) BETWEEN 1 AND 500), scope jsonb NOT NULL,
 created_by uuid NOT NULL REFERENCES users(id), created_at timestamptz NOT NULL DEFAULT now(), CHECK(from_user<>to_user)
);
CREATE TABLE ai_extraction_attempts (
 id bigserial PRIMARY KEY, extraction_id uuid NOT NULL REFERENCES ai_extractions(id),
 actor_id uuid NOT NULL REFERENCES users(id), created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ai_extraction_attempts_actor_idx ON ai_extraction_attempts(actor_id,created_at);
