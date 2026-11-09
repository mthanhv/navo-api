-- Field notes and follow-up tasks (REQUIREMENTS 5.10, mockups Sales hôm nay / Ghi nhanh).
-- A note carries the device's id, so a draft written offline and sent twice is stored once.
CREATE TABLE notes (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id   uuid NOT NULL UNIQUE,
    customer_id uuid NOT NULL REFERENCES organizations (id),
    contract_id uuid REFERENCES contracts (id),
    body        text NOT NULL CHECK (length(btrim(body)) BETWEEN 1 AND 5000),
    written_at  timestamptz NOT NULL,
    author_id   uuid NOT NULL REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notes_customer_idx ON notes (customer_id, written_at DESC);

CREATE TABLE tasks (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    title       text NOT NULL CHECK (length(btrim(title)) BETWEEN 1 AND 300),
    due_on      date,
    owner_id    uuid NOT NULL REFERENCES users (id),
    customer_id uuid REFERENCES organizations (id),
    contract_id uuid REFERENCES contracts (id),
    note_id     uuid REFERENCES notes (id),
    done_at     timestamptz,
    created_by  uuid NOT NULL REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX tasks_owner_open_idx ON tasks (owner_id, due_on) WHERE done_at IS NULL;

ALTER TABLE attachment_links DROP CONSTRAINT attachment_links_owner_type_check,
    ADD CONSTRAINT attachment_links_owner_type_check CHECK (owner_type IN ('customer', 'contract', 'purchase_order', 'goods_receipt',
        'delivery_order', 'sales_invoice', 'purchase_invoice', 'receipt', 'payment', 'note'));
