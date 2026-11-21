#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/clear-transactional-data.sh [--execute --confirm-db NAME] [--url-env DATABASE_URL|TEST_DATABASE_URL]

Without --execute, prints a dry-run count and changes nothing.
Execution requires the exact current database name and only allows names marked dev, test, or local.
EOF
}

mode=dry-run
url_env=DATABASE_URL
confirm_db=

while (($#)); do
  case "$1" in
    --help|-h)
      usage
      exit 0
      ;;
    --execute)
      mode=execute
      shift
      ;;
    --dry-run)
      mode=dry-run
      shift
      ;;
    --url-env)
      if (($# < 2)); then
        echo "--url-env requires DATABASE_URL or TEST_DATABASE_URL" >&2
        exit 2
      fi
      url_env=$2
      shift 2
      ;;
    --confirm-db)
      if (($# < 2)); then
        echo "--confirm-db requires the exact database name" >&2
        exit 2
      fi
      confirm_db=$2
      shift 2
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

case "$url_env" in
  DATABASE_URL) database_url=${DATABASE_URL:-} ;;
  TEST_DATABASE_URL) database_url=${TEST_DATABASE_URL:-} ;;
  *)
    echo "--url-env must be DATABASE_URL or TEST_DATABASE_URL" >&2
    exit 2
    ;;
esac

if [[ -z "$database_url" ]]; then
  echo "$url_env is not set" >&2
  exit 2
fi
if ! command -v psql >/dev/null 2>&1; then
  echo "psql is required" >&2
  exit 2
fi
if [[ "$mode" == dry-run && -n "$confirm_db" ]]; then
  echo "--confirm-db is only valid with --execute" >&2
  exit 2
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
project_dir=$(cd -- "$script_dir/.." && pwd)
cd "$project_dir"

database_name=$(psql "$database_url" -X -v ON_ERROR_STOP=1 -At -c 'SELECT current_database()')
database_name_lower=$(printf '%s' "$database_name" | tr '[:upper:]' '[:lower:]')
if [[ "$database_name_lower" == *prod* || "$database_name_lower" == *live* ]]; then
  echo "Refusing to run against a production/live database: $database_name" >&2
  exit 2
fi
if [[ ! "$database_name_lower" =~ (^|[_-])(dev|development|test|local)([_-]|$) ]]; then
  echo "Refusing database without a dev/test/local name marker: $database_name" >&2
  exit 2
fi
if [[ "$mode" == execute && "$confirm_db" != "$database_name" ]]; then
  echo "Execution requires --confirm-db '$database_name'" >&2
  exit 2
fi

printf 'Database: %s\n' "$database_name"
printf 'Mode: %s\n' "$mode"

psql "$database_url" -X -v ON_ERROR_STOP=1 -P pager=off <<'SQL'
SELECT table_name, row_count
FROM (
    SELECT 'quotations' AS table_name, count(*) AS row_count FROM quotations
    UNION ALL SELECT 'quotation_lines', count(*) FROM quotation_lines
    UNION ALL SELECT 'contracts', count(*) FROM contracts
    UNION ALL SELECT 'contract_lines', count(*) FROM contract_lines
    UNION ALL SELECT 'contract_status_changes', count(*) FROM contract_status_changes
    UNION ALL SELECT 'contract_amendments', count(*) FROM contract_amendments
    UNION ALL SELECT 'contract_amendment_lines', count(*) FROM contract_amendment_lines
    UNION ALL SELECT 'payment_milestones', count(*) FROM payment_milestones
    UNION ALL SELECT 'payment_term_history', count(*) FROM payment_term_history
    UNION ALL SELECT 'sales_invoices', count(*) FROM sales_invoices
    UNION ALL SELECT 'receipts', count(*) FROM receipts
    UNION ALL SELECT 'receipt_allocations', count(*) FROM receipt_allocations
    UNION ALL SELECT 'purchase_invoices', count(*) FROM purchase_invoices
    UNION ALL SELECT 'purchase_invoice_orders', count(*) FROM purchase_invoice_orders
    UNION ALL SELECT 'payments', count(*) FROM payments
    UNION ALL SELECT 'payment_allocations', count(*) FROM payment_allocations
    UNION ALL SELECT 'misa_exports', count(*) FROM misa_exports
    UNION ALL SELECT 'purchase_orders', count(*) FROM purchase_orders
    UNION ALL SELECT 'purchase_order_lines', count(*) FROM purchase_order_lines
    UNION ALL SELECT 'purchase_order_closures', count(*) FROM purchase_order_closures
    UNION ALL SELECT 'goods_receipts', count(*) FROM goods_receipts
    UNION ALL SELECT 'goods_receipt_lines', count(*) FROM goods_receipt_lines
    UNION ALL SELECT 'delivery_orders', count(*) FROM delivery_orders
    UNION ALL SELECT 'delivery_order_lines', count(*) FROM delivery_order_lines
    UNION ALL SELECT 'service_visits', count(*) FROM service_visits
    UNION ALL SELECT 'service_visit_lines', count(*) FROM service_visit_lines
    UNION ALL SELECT 'contract_equipment', count(*) FROM contract_equipment
    UNION ALL SELECT 'acceptances', count(*) FROM acceptances
    UNION ALL SELECT 'acceptance_lines', count(*) FROM acceptance_lines
    UNION ALL SELECT 'customer_returns', count(*) FROM customer_returns
    UNION ALL SELECT 'customer_return_lines', count(*) FROM customer_return_lines
    UNION ALL SELECT 'supplier_returns', count(*) FROM supplier_returns
    UNION ALL SELECT 'supplier_return_lines', count(*) FROM supplier_return_lines
    UNION ALL SELECT 'quantity_override_requests', count(*) FROM quantity_override_requests
    UNION ALL SELECT 'document_reversals', count(*) FROM document_reversals
    UNION ALL SELECT 'transaction_attachment_links', count(*) FROM attachment_links
        WHERE owner_type IN ('contract', 'purchase_order', 'goods_receipt', 'delivery_order',
                             'sales_invoice', 'purchase_invoice', 'receipt', 'payment',
                             'quotation', 'service_visit')
    UNION ALL SELECT 'transaction_files_with_no_other_links', count(DISTINCT l.file_id)
        FROM attachment_links l
        WHERE l.owner_type IN ('contract', 'purchase_order', 'goods_receipt', 'delivery_order',
                               'sales_invoice', 'purchase_invoice', 'receipt', 'payment',
                               'quotation', 'service_visit')
          AND NOT EXISTS (
              SELECT 1 FROM attachment_links other
              WHERE other.file_id = l.file_id
                AND other.owner_type NOT IN ('contract', 'purchase_order', 'goods_receipt', 'delivery_order',
                                             'sales_invoice', 'purchase_invoice', 'receipt', 'payment',
                                             'quotation', 'service_visit')
          )
    UNION ALL SELECT 'idempotency_records', count(*) FROM idempotency_records
) AS cleanup_counts
ORDER BY table_name;
SQL

if [[ "$mode" == dry-run ]]; then
  echo "Dry run only. Re-run with --execute --confirm-db '$database_name' to delete these records."
  exit 0
fi

removed_file_ids=$(mktemp "${TMPDIR:-/tmp}/navo-transaction-cleanup.XXXXXX")
trap 'rm -f -- "$removed_file_ids"' EXIT

psql "$database_url" -X -v ON_ERROR_STOP=1 -qAt >"$removed_file_ids" <<'SQL'
BEGIN;

CREATE TEMP TABLE transaction_cleanup_file_ids (id uuid PRIMARY KEY) ON COMMIT PRESERVE ROWS;
CREATE TEMP TABLE transaction_cleanup_removed_file_ids (id uuid PRIMARY KEY) ON COMMIT PRESERVE ROWS;

INSERT INTO transaction_cleanup_file_ids (id)
SELECT DISTINCT l.file_id
FROM attachment_links l
WHERE l.owner_type IN ('contract', 'purchase_order', 'goods_receipt', 'delivery_order',
                       'sales_invoice', 'purchase_invoice', 'receipt', 'payment',
                       'quotation', 'service_visit')
  AND NOT EXISTS (
      SELECT 1 FROM attachment_links other
      WHERE other.file_id = l.file_id
        AND other.owner_type NOT IN ('contract', 'purchase_order', 'goods_receipt', 'delivery_order',
                                     'sales_invoice', 'purchase_invoice', 'receipt', 'payment',
                                     'quotation', 'service_visit')
  );

DELETE FROM attachment_links
WHERE owner_type IN ('contract', 'purchase_order', 'goods_receipt', 'delivery_order',
                     'sales_invoice', 'purchase_invoice', 'receipt', 'payment',
                     'quotation', 'service_visit');
UPDATE attachment_links SET contract_id = NULL WHERE owner_type = 'note' AND contract_id IS NOT NULL;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE NOT tgisinternal
          AND tgrelid IN (
              'contracts'::regclass, 'purchase_orders'::regclass, 'goods_receipts'::regclass,
              'delivery_orders'::regclass, 'document_reversals'::regclass,
              'contract_status_changes'::regclass, 'contract_amendments'::regclass,
              'contract_amendment_lines'::regclass, 'acceptances'::regclass,
              'acceptance_lines'::regclass, 'customer_returns'::regclass,
              'customer_return_lines'::regclass, 'supplier_returns'::regclass,
              'supplier_return_lines'::regclass, 'purchase_order_closures'::regclass
          )
          AND tgenabled <> 'O'
    ) THEN
        RAISE EXCEPTION 'Non-default trigger state found; refusing cleanup';
    END IF;
END $$;

ALTER TABLE contracts DISABLE TRIGGER USER;
ALTER TABLE purchase_orders DISABLE TRIGGER USER;
ALTER TABLE goods_receipts DISABLE TRIGGER USER;
ALTER TABLE delivery_orders DISABLE TRIGGER USER;
ALTER TABLE document_reversals DISABLE TRIGGER USER;
ALTER TABLE contract_status_changes DISABLE TRIGGER USER;
ALTER TABLE contract_amendments DISABLE TRIGGER USER;
ALTER TABLE contract_amendment_lines DISABLE TRIGGER USER;
ALTER TABLE acceptances DISABLE TRIGGER USER;
ALTER TABLE acceptance_lines DISABLE TRIGGER USER;
ALTER TABLE customer_returns DISABLE TRIGGER USER;
ALTER TABLE customer_return_lines DISABLE TRIGGER USER;
ALTER TABLE supplier_returns DISABLE TRIGGER USER;
ALTER TABLE supplier_return_lines DISABLE TRIGGER USER;
ALTER TABLE purchase_order_closures DISABLE TRIGGER USER;

DELETE FROM idempotency_records;
DELETE FROM quantity_override_requests;
DELETE FROM document_reversals;
DELETE FROM contract_status_changes;
DELETE FROM contract_amendment_lines;
DELETE FROM contract_amendments;
DELETE FROM purchase_order_closures;
DELETE FROM payment_allocations;
DELETE FROM receipt_allocations;
DELETE FROM sales_invoices;
DELETE FROM payment_term_history;
DELETE FROM payments;
DELETE FROM purchase_invoice_orders;
DELETE FROM purchase_invoices;
DELETE FROM receipts;
DELETE FROM payment_milestones;
DELETE FROM acceptance_lines;
DELETE FROM acceptances;
DELETE FROM customer_return_lines;
DELETE FROM customer_returns;
DELETE FROM supplier_return_lines;
DELETE FROM supplier_returns;
DELETE FROM service_visit_lines;
DELETE FROM service_visits;
DELETE FROM contract_equipment;
DELETE FROM goods_receipt_lines;
DELETE FROM goods_receipts;
DELETE FROM delivery_order_lines;
DELETE FROM delivery_orders;
DELETE FROM purchase_order_lines;
DELETE FROM purchase_orders;
DELETE FROM contract_lines;
DELETE FROM quotation_lines;
DELETE FROM quotations;
DELETE FROM contracts;
DELETE FROM misa_exports;

WITH removed AS (
    DELETE FROM files f
    USING transaction_cleanup_file_ids c
    WHERE f.id = c.id
      AND NOT EXISTS (SELECT 1 FROM attachment_links l WHERE l.file_id = f.id)
    RETURNING f.id
)
INSERT INTO transaction_cleanup_removed_file_ids (id)
SELECT id FROM removed;

ALTER TABLE contracts ENABLE TRIGGER USER;
ALTER TABLE purchase_orders ENABLE TRIGGER USER;
ALTER TABLE goods_receipts ENABLE TRIGGER USER;
ALTER TABLE delivery_orders ENABLE TRIGGER USER;
ALTER TABLE document_reversals ENABLE TRIGGER USER;
ALTER TABLE contract_status_changes ENABLE TRIGGER USER;
ALTER TABLE contract_amendments ENABLE TRIGGER USER;
ALTER TABLE contract_amendment_lines ENABLE TRIGGER USER;
ALTER TABLE acceptances ENABLE TRIGGER USER;
ALTER TABLE acceptance_lines ENABLE TRIGGER USER;
ALTER TABLE customer_returns ENABLE TRIGGER USER;
ALTER TABLE customer_return_lines ENABLE TRIGGER USER;
ALTER TABLE supplier_returns ENABLE TRIGGER USER;
ALTER TABLE supplier_return_lines ENABLE TRIGGER USER;
ALTER TABLE purchase_order_closures ENABLE TRIGGER USER;

COMMIT;

SELECT id FROM transaction_cleanup_removed_file_ids ORDER BY id;
SQL

files_dir=${NAVO_FILES_DIR:-var/files}
if [[ "$files_dir" != /* ]]; then
  files_dir="$project_dir/$files_dir"
fi

removed_files=0
while IFS= read -r file_id; do
  [[ -n "$file_id" ]] || continue
  if [[ ! "$file_id" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]]; then
    echo "Unexpected file id returned by database; cleanup committed, file removal stopped" >&2
    exit 1
  fi
  file_path="$files_dir/${file_id:0:2}/$file_id"
  if [[ -e "$file_path" || -L "$file_path" ]]; then
    if ! rm -f -- "$file_path"; then
      echo "Database cleanup committed, but could not remove stored file: $file_path" >&2
      exit 1
    fi
    removed_files=$((removed_files + 1))
  fi
done <"$removed_file_ids"

printf 'Cleanup complete. Removed %d transaction-only file(s).\n' "$removed_files"
