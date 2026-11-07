-- Foundation: users, roles and grants, sessions, audit, idempotency, numbering, company settings.
-- Navo serves one company per installation (REQUIREMENTS 1.2): no tenant_id, no RLS.

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text NOT NULL CHECK (email ~ '^[^@\s]+@[^@\s]+\.[^@\s]+$' AND length(email) <= 254),
    full_name     text NOT NULL CHECK (length(btrim(full_name)) BETWEEN 1 AND 200),
    password_hash text NOT NULL,
    is_active     boolean NOT NULL DEFAULT true,
    version       bigint NOT NULL DEFAULT 1,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_key ON users (lower(email));

CREATE TABLE user_roles (
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role    text NOT NULL CHECK (role IN ('sales', 'leader', 'accounting', 'admin')),
    PRIMARY KEY (user_id, role)
);

-- Sensitive permissions granted one by one (purchase_cost.read, finance.read, quantity.override, ...).
CREATE TABLE user_grants (
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    grant_code text NOT NULL CHECK (grant_code ~ '^[a-z_]+\.[a-z_]+$'),
    granted_by uuid REFERENCES users (id),
    granted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, grant_code)
);

-- Opaque session tokens; only their SHA-256 is stored.
CREATE TABLE sessions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE,
    client     text NOT NULL CHECK (client IN ('web', 'mobile')),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);
CREATE INDEX sessions_user_idx ON sessions (user_id);

CREATE TABLE audit_logs (
    id          bigserial PRIMARY KEY,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    actor_id    uuid REFERENCES users (id),
    request_id  text,
    action      text NOT NULL,
    entity_type text NOT NULL,
    entity_id   uuid,
    changes     jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX audit_logs_entity_idx ON audit_logs (entity_type, entity_id);

-- Retried confirm commands return the first result instead of acting twice.
CREATE TABLE idempotency_records (
    actor_id     uuid NOT NULL REFERENCES users (id),
    scope        text NOT NULL,
    key          text NOT NULL CHECK (length(key) BETWEEN 8 AND 128),
    request_hash bytea NOT NULL,
    status_code  int,
    response     jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (actor_id, scope, key)
);

-- Yearly counters for documents not numbered under a contract (REQUIREMENTS 5.12).
CREATE TABLE document_series (
    doc_type   text NOT NULL CHECK (doc_type IN ('SC', 'PI', 'RV', 'PV', 'DRAFT')),
    year       int  NOT NULL,
    last_value int  NOT NULL,
    PRIMARY KEY (doc_type, year)
);

CREATE TABLE company_settings (
    id                 boolean PRIMARY KEY DEFAULT true CHECK (id),
    name               text NOT NULL,
    timezone           text NOT NULL DEFAULT 'Asia/Ho_Chi_Minh',
    alert_horizon_days int  NOT NULL DEFAULT 14 CHECK (alert_horizon_days BETWEEN 1 AND 90)
);
INSERT INTO company_settings (name) VALUES ('Nhật Duy SGN');
