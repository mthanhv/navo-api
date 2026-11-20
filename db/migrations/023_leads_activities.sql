-- Leads, sales activities and the sales manager role (REQUIREMENTS 5.14, owner answers 1.9, 2026-11-20).

-- 1. Sales manager: a whole-company sales role (not finance, not users).
ALTER TABLE user_roles DROP CONSTRAINT user_roles_role_check;
ALTER TABLE user_roles ADD CONSTRAINT user_roles_role_check CHECK (role IN ('sales', 'sales_manager', 'delivery', 'accounting', 'admin'));

-- 2. Leads are organizations that have not bought yet: same contacts, activities and quotations,
-- so winning one copies nothing. A lead becomes a customer with its first contract (or by hand).
ALTER TABLE organizations
    ADD COLUMN lead_stage      text CHECK (lead_stage IN ('new', 'contacted', 'qualified', 'quoted', 'won', 'lost', 'later')),
    ADD COLUMN lead_source     text CHECK (lead_source IS NULL OR lead_source IN ('referral', 'trade_show', 'website', 'cold_call', 'zalo', 'existing_customer', 'other')),
    ADD COLUMN industry        text CHECK (industry IS NULL OR length(industry) <= 80),
    ADD COLUMN interest        text CHECK (interest IS NULL OR length(interest) <= 1000),
    ADD COLUMN estimated_value numeric(24, 2) CHECK (estimated_value IS NULL OR estimated_value >= 0),
    ADD COLUMN lost_reason     text CHECK (lost_reason IS NULL OR length(lost_reason) <= 500),
    ADD COLUMN remind_on       date,
    ADD COLUMN lead_since      timestamptz,
    ADD COLUMN won_at          timestamptz,
    ADD CONSTRAINT organizations_lost_reason CHECK (lead_stage IS DISTINCT FROM 'lost' OR lost_reason IS NOT NULL);
ALTER TABLE organizations DROP CONSTRAINT organizations_check;
ALTER TABLE organizations ADD CONSTRAINT organizations_role_check CHECK (is_customer OR is_supplier OR lead_stage IS NOT NULL);
CREATE INDEX organizations_lead_idx ON organizations (lead_stage, owner_id) WHERE lead_stage IS NOT NULL;

-- 3. Activities: one log for calls, visits, emails, SMS, Zalo, notes and to-dos. Notes and tasks
-- move in with the same ids (files attached to a note stay attached; owner_type 'note' = activity).
CREATE TABLE activities (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id       uuid UNIQUE, -- offline drafts from the phone are stored once
    kind            text NOT NULL CHECK (kind IN ('call', 'visit', 'email', 'sms', 'zalo', 'note', 'task')),
    status          text NOT NULL CHECK (status IN ('planned', 'done')),
    organization_id uuid REFERENCES organizations (id),
    contact_id      uuid REFERENCES contacts (id) ON DELETE SET NULL,
    contract_id     uuid REFERENCES contracts (id) ON DELETE SET NULL,
    quotation_id    uuid REFERENCES quotations (id) ON DELETE SET NULL,
    parent_id       uuid REFERENCES activities (id) ON DELETE SET NULL, -- the activity whose "next step" this is
    summary         text NOT NULL CHECK (length(btrim(summary)) BETWEEN 1 AND 5000),
    outcome         text CHECK (outcome IS NULL OR outcome IN ('reached', 'not_reached')),
    planned_on      date,
    done_at         timestamptz,
    -- Visit check-in: where the phone was when the visit was recorded (owner decision: GPS yes).
    check_lat       double precision CHECK (check_lat IS NULL OR check_lat BETWEEN -90 AND 90),
    check_lng       double precision CHECK (check_lng IS NULL OR check_lng BETWEEN -180 AND 180),
    check_accuracy  double precision CHECK (check_accuracy IS NULL OR check_accuracy >= 0),
    check_at        timestamptz,
    auto            boolean NOT NULL DEFAULT false, -- written by Navo (quotation sent, email sent, contract signed)
    owner_id        uuid NOT NULL REFERENCES users (id),
    created_by      uuid NOT NULL REFERENCES users (id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'done') = (done_at IS NOT NULL)),
    CHECK ((check_lat IS NULL) = (check_lng IS NULL))
);
CREATE INDEX activities_org_done_idx ON activities (organization_id, done_at DESC) WHERE status = 'done';
CREATE INDEX activities_org_planned_idx ON activities (organization_id, planned_on, created_at) WHERE status = 'planned';
CREATE INDEX activities_owner_planned_idx ON activities (owner_id, planned_on) WHERE status = 'planned';
CREATE INDEX activities_owner_done_idx ON activities (owner_id, done_at) WHERE status = 'done';
CREATE INDEX activities_contract_idx ON activities (contract_id) WHERE contract_id IS NOT NULL;

INSERT INTO activities (id, client_id, kind, status, organization_id, contract_id, summary, done_at, owner_id, created_by, created_at)
SELECT id, client_id, 'note', 'done', customer_id, contract_id, body, written_at, author_id, author_id, created_at FROM notes;
INSERT INTO activities (id, kind, status, organization_id, contract_id, parent_id, summary, planned_on, done_at, owner_id, created_by, created_at)
SELECT id, 'task', CASE WHEN done_at IS NULL THEN 'planned' ELSE 'done' END, customer_id, contract_id, note_id, title, due_on, done_at, owner_id, created_by, created_at FROM tasks;
DROP TABLE tasks;
DROP TABLE notes;
