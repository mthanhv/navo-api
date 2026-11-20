ALTER TABLE organizations
    ADD COLUMN invoice_company_name text CHECK (invoice_company_name IS NULL OR length(invoice_company_name) <= 300),
    ADD COLUMN invoice_tax_code text CHECK (invoice_tax_code IS NULL OR invoice_tax_code ~ '^[0-9]{10}(-[0-9]{3})?$'),
    ADD COLUMN invoice_address text CHECK (invoice_address IS NULL OR length(invoice_address) <= 300),
    ADD COLUMN invoice_email text CHECK (invoice_email IS NULL OR length(invoice_email) <= 254);
