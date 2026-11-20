-- Settings page and Zalo Bot notifications (owner decisions 2026-11-20, REQUIREMENTS 5.15 / 1.9).
-- Company details print on quotations. The bot token is kept like the other secrets in this
-- database and never returned by the API (only its last 4 characters).
ALTER TABLE company_settings
    ADD COLUMN tax_code text CHECK (tax_code IS NULL OR tax_code ~ '^[0-9]{10}(-[0-9]{3})?$'),
    ADD COLUMN address  text CHECK (address IS NULL OR length(address) <= 300),
    ADD COLUMN phone    text CHECK (phone IS NULL OR length(phone) <= 40),
    ADD COLUMN email    text CHECK (email IS NULL OR length(email) <= 254);

CREATE TABLE notification_settings (
    id                boolean PRIMARY KEY DEFAULT true CHECK (id),
    zalo_token        text CHECK (zalo_token IS NULL OR length(zalo_token) BETWEEN 10 AND 300),
    zalo_bot_name     text,
    zalo_connected_at timestamptz,
    -- Daily messages go out at digest_time (Monday–Saturday). Immediate messages wait out quiet hours.
    digest_time       time NOT NULL DEFAULT '08:00',
    quiet_from        time NOT NULL DEFAULT '20:00',
    quiet_to          time NOT NULL DEFAULT '07:00',
    last_digest_on    date,
    -- Last getUpdates offset read from Zalo, so a message is never handled twice.
    update_offset     bigint NOT NULL DEFAULT 0,
    last_error        text,
    last_error_at     timestamptz,
    updated_at        timestamptz NOT NULL DEFAULT now()
);
INSERT INTO notification_settings DEFAULT VALUES;

-- Where messages go: a person who linked their Zalo, or a group the bot was @mentioned in.
CREATE TABLE zalo_chats (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    chat_id    text NOT NULL UNIQUE CHECK (length(chat_id) BETWEEN 1 AND 100),
    kind       text NOT NULL CHECK (kind IN ('person', 'group')),
    title      text NOT NULL CHECK (length(btrim(title)) BETWEEN 1 AND 120),
    user_id    uuid UNIQUE REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((kind = 'person') = (user_id IS NOT NULL))
);

-- A person links their Zalo by sending this code to the bot (valid 15 minutes).
CREATE TABLE zalo_link_codes (
    code       text PRIMARY KEY CHECK (code ~ '^[0-9]{6}$'),
    user_id    uuid NOT NULL UNIQUE REFERENCES users (id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL
);

CREATE TABLE notification_rules (
    key       text PRIMARY KEY CHECK (key IN ('attention_digest', 'override_requested', 'deliveries_tomorrow', 'quotation_expiring')),
    enabled   boolean NOT NULL DEFAULT true,
    -- to_people: each concerned person on their linked Zalo; group_id: also post to this group.
    to_people boolean NOT NULL DEFAULT true,
    group_id  uuid REFERENCES zalo_chats (id) ON DELETE SET NULL
);
INSERT INTO notification_rules (key) VALUES ('attention_digest'), ('override_requested'), ('deliveries_tomorrow'), ('quotation_expiring');

-- The outbox and send log: each message once (dedupe_key), retried a few times, then failed.
CREATE TABLE notifications (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    dedupe_key text NOT NULL UNIQUE,
    rule       text NOT NULL,
    chat_id    uuid NOT NULL REFERENCES zalo_chats (id) ON DELETE CASCADE,
    body       text NOT NULL CHECK (length(body) BETWEEN 1 AND 2000),
    status     text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed')),
    attempts   int NOT NULL DEFAULT 0,
    error      text,
    created_at timestamptz NOT NULL DEFAULT now(),
    sent_at    timestamptz
);
CREATE INDEX notifications_pending_idx ON notifications (created_at) WHERE status = 'pending';
