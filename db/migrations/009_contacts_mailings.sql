-- People at a customer or supplier (REQUIREMENTS 5.1), and product-news emails sent to
-- customer contacts. A partner can have many contacts; one may be marked as the main one.
CREATE TABLE contacts (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES organizations (id),
    full_name       text NOT NULL CHECK (length(btrim(full_name)) BETWEEN 1 AND 200),
    job_title       text CHECK (job_title IS NULL OR length(job_title) <= 120),
    email           text CHECK (email IS NULL OR (length(email) <= 254 AND email ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$')),
    phone           text CHECK (phone IS NULL OR length(phone) <= 40),
    note            text CHECK (note IS NULL OR length(note) <= 500),
    is_primary      boolean NOT NULL DEFAULT false,
    -- false once the person asks not to receive product news; mailings skip them.
    accepts_email   boolean NOT NULL DEFAULT true,
    version         bigint NOT NULL DEFAULT 1,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX contacts_organization_idx ON contacts (organization_id, full_name);
CREATE UNIQUE INDEX contacts_one_primary_key ON contacts (organization_id) WHERE is_primary;
CREATE UNIQUE INDEX contacts_organization_email_key ON contacts (organization_id, lower(email)) WHERE email IS NOT NULL;

-- One email written once and sent to each chosen contact separately.
CREATE TABLE mailings (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subject     text NOT NULL CHECK (length(btrim(subject)) BETWEEN 1 AND 200 AND subject !~ '[\r\n]'),
    body        text NOT NULL CHECK (length(btrim(body)) BETWEEN 1 AND 10000),
    created_by  uuid NOT NULL REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);
CREATE INDEX mailings_created_idx ON mailings (created_at DESC);

-- Products named in the email, with the names as they were when it was sent.
CREATE TABLE mailing_products (
    mailing_id        uuid NOT NULL REFERENCES mailings (id) ON DELETE CASCADE,
    product_id        uuid NOT NULL REFERENCES products (id),
    position          int NOT NULL,
    name              text NOT NULL,
    manufacturer      text,
    manufacturer_code text,
    PRIMARY KEY (mailing_id, product_id)
);

-- Who it went to. Name and address are kept as sent, even if the contact changes later.
CREATE TABLE mailing_recipients (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    mailing_id      uuid NOT NULL REFERENCES mailings (id) ON DELETE CASCADE,
    contact_id      uuid REFERENCES contacts (id) ON DELETE SET NULL,
    organization_id uuid NOT NULL REFERENCES organizations (id),
    full_name       text NOT NULL,
    email           text NOT NULL,
    status          text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'sent', 'failed')),
    error           text,
    sent_at         timestamptz,
    UNIQUE (mailing_id, email)
);
CREATE INDEX mailing_recipients_contact_idx ON mailing_recipients (contact_id);
