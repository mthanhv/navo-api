-- Two-step sign-in (TOTP, RFC 6238) with one-time recovery codes. The secret is kept in the
-- database like the password hash; backups are encrypted at rest (KMS). totp_last_step
-- stops a code being used twice.
ALTER TABLE users
    ADD COLUMN totp_secret     text,
    ADD COLUMN totp_pending    text,
    ADD COLUMN totp_enabled_at timestamptz,
    ADD COLUMN totp_last_step  bigint NOT NULL DEFAULT 0,
    ADD CONSTRAINT users_totp_enabled_check CHECK ((totp_secret IS NULL) = (totp_enabled_at IS NULL));

CREATE TABLE user_recovery_codes (
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash bytea NOT NULL,
    used_at   timestamptz,
    PRIMARY KEY (user_id, code_hash)
);
