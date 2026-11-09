-- Files and where they belong (REQUIREMENTS 5.12, TECHNICAL_DESIGN 7.4). A file is stored
-- once, by id, outside the database; links attach it to a customer or a document. The
-- folder tree is a query over the links, so nothing can be "filed in the wrong folder".
CREATE TABLE files (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    original_name text NOT NULL CHECK (length(original_name) BETWEEN 1 AND 255),
    content_type  text NOT NULL,
    size_bytes    bigint NOT NULL CHECK (size_bytes > 0),
    sha256        bytea NOT NULL CHECK (length(sha256) = 32),
    uploaded_by   uuid NOT NULL REFERENCES users (id),
    uploaded_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE attachment_links (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    file_id     uuid NOT NULL REFERENCES files (id),
    owner_type  text NOT NULL CHECK (owner_type IN ('customer', 'contract', 'purchase_order', 'goods_receipt', 'delivery_order',
                                                    'sales_invoice', 'purchase_invoice', 'receipt', 'payment')),
    owner_id    uuid NOT NULL,
    -- Denormalised for scope checks and the tree; NULL contract for customer-level files.
    customer_id uuid REFERENCES organizations (id),
    contract_id uuid REFERENCES contracts (id),
    doc_kind    text NOT NULL CHECK (doc_kind ~ '^[a-z_]{2,40}$'),
    created_by  uuid NOT NULL REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (file_id, owner_type, owner_id)
);
CREATE INDEX attachment_links_owner_idx ON attachment_links (owner_type, owner_id);
CREATE INDEX attachment_links_contract_idx ON attachment_links (contract_id);
CREATE INDEX attachment_links_customer_idx ON attachment_links (customer_id) WHERE contract_id IS NULL;
CREATE INDEX files_name_idx ON files (lower(original_name));
