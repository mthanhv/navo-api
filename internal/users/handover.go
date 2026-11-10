package users

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/auth"
	"navo-api/internal/httpx"
	"navo-api/internal/response"
)

func (h *Handlers) HandoverPreview(c *gin.Context) {
	from, err := uuid.Parse(c.Query("from_user"))
	if err != nil {
		response.Fail(c, apperrors.Invalid("from_user", "uuid", nil))
		return
	}
	ctx := c.Request.Context()
	rows, err := h.Pool.Query(ctx, `SELECT id,coalesce(number,draft_number),title,version FROM contracts WHERE owner_id=$1 AND status IN ('draft','active') ORDER BY id`, from)
	if err != nil {
		response.Fail(c, err)
		return
	}
	contracts := []gin.H{}
	for rows.Next() {
		var id uuid.UUID
		var number, title string
		var version int64
		if err := rows.Scan(&id, &number, &title, &version); err != nil {
			rows.Close()
			response.Fail(c, err)
			return
		}
		contracts = append(contracts, gin.H{"id": id, "number": number, "title": title, "version": version})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	rows, err = h.Pool.Query(ctx, `SELECT id,title FROM tasks WHERE owner_id=$1 AND done_at IS NULL ORDER BY id`, from)
	if err != nil {
		response.Fail(c, err)
		return
	}
	defer rows.Close()
	tasks := []gin.H{}
	for rows.Next() {
		var id uuid.UUID
		var title string
		if err := rows.Scan(&id, &title); err != nil {
			response.Fail(c, err)
			return
		}
		tasks = append(tasks, gin.H{"id": id, "title": title})
	}
	if err := rows.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"contracts": contracts, "tasks": tasks})
}

func (h *Handlers) Handover(c *gin.Context) {
	var in struct {
		From      string `json:"from_user"`
		To        string `json:"to_user"`
		Reason    string `json:"reason"`
		Contracts []struct {
			ID      string `json:"id"`
			Version int64  `json:"version"`
		} `json:"contracts"`
		Tasks []string `json:"task_ids"`
	}
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	from, to := httpx.UUID(&errs, "from_user", in.From), httpx.UUID(&errs, "to_user", in.To)
	in.Reason = strings.TrimSpace(in.Reason)
	if from == to {
		errs.Add("to_user", "different_user", nil)
	}
	if in.Reason == "" || len([]rune(in.Reason)) > 500 {
		errs.Add("reason", "required", nil)
	}
	ids := []uuid.UUID{}
	versions := map[uuid.UUID]int64{}
	for _, x := range in.Contracts {
		id := httpx.UUID(&errs, "contracts", x.ID)
		if _, ok := versions[id]; ok {
			errs.Add("contracts", "duplicate", nil)
		}
		ids = append(ids, id)
		versions[id] = x.Version
	}
	tasks := []uuid.UUID{}
	for _, x := range in.Tasks {
		tasks = append(tasks, httpx.UUID(&errs, "task_ids", x))
	}
	if len(ids) == 0 && len(tasks) == 0 {
		errs.Add("contracts", "required", nil)
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "users.handover", true, 201, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		if _, err := tx.Exec(ctx, `SELECT id FROM users WHERE id=ANY($1::uuid[]) ORDER BY id FOR UPDATE`, []uuid.UUID{from, to}); err != nil {
			return nil, err
		}
		var eligible bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users u WHERE u.id=$1 AND u.is_active AND EXISTS(SELECT 1 FROM user_roles WHERE user_id=u.id AND role IN ('sales','admin')))`, to).Scan(&eligible); err != nil {
			return nil, err
		}
		if !eligible {
			return nil, apperrors.Invalid("to_user", "not_found", nil)
		}
		rows, err := tx.Query(ctx, `SELECT id,version FROM contracts WHERE owner_id=$1 AND id=ANY($2) AND status IN ('draft','active') ORDER BY id FOR UPDATE`, from, ids)
		if err != nil {
			return nil, err
		}
		count := 0
		for rows.Next() {
			var id uuid.UUID
			var v int64
			if err := rows.Scan(&id, &v); err != nil {
				rows.Close()
				return nil, err
			}
			if versions[id] != v {
				rows.Close()
				return nil, apperrors.Conflict("version_conflict", "Handover preview changed. Reload it.", nil)
			}
			count++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if count != len(ids) {
			return nil, apperrors.Conflict("version_conflict", "The selected assignments changed. Reload the preview.", nil)
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM tasks WHERE id=ANY($1::uuid[]) ORDER BY id FOR UPDATE`, tasks); err != nil {
			return nil, err
		}
		// A task linked to an unselected contract cannot be moved independently to a
		// salesperson who could not see it after the handover.
		var invalid bool
		if err := tx.QueryRow(ctx, `SELECT count(*)<>$3 OR coalesce(bool_or(contract_id IS NOT NULL AND contract_id<>ALL($4::uuid[]) AND NOT EXISTS(SELECT 1 FROM contracts c WHERE c.id=t.contract_id AND c.owner_id=$2)),false) FROM tasks t WHERE t.id=ANY($1::uuid[]) AND t.owner_id=$5 AND t.done_at IS NULL`, tasks, to, len(tasks), ids, from).Scan(&invalid); err != nil {
			return nil, err
		}
		if invalid {
			return nil, apperrors.Conflict("version_conflict", "The selected tasks changed or belong to an unselected contract.", nil)
		}
		if _, err := tx.Exec(ctx, `UPDATE contracts SET owner_id=$2,version=version+1,updated_at=now() WHERE id=ANY($1)`, ids, to); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET owner_id=$2 WHERE id=ANY($1)`, tasks, to); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE organizations SET owner_id=$2,version=version+1 WHERE owner_id=$1 AND id IN (SELECT customer_id FROM contracts WHERE id=ANY($3))`, from, to, ids); err != nil {
			return nil, err
		}
		// Invalidate historical command replay under the old assignment scope.
		if _, err := tx.Exec(ctx, `UPDATE users SET version=version+1 WHERE id=ANY($1::uuid[])`, []uuid.UUID{from, to}); err != nil {
			return nil, err
		}
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO handovers(from_user,to_user,reason,scope,created_by) VALUES($1,$2,$3,$4,$5) RETURNING id`, from, to, in.Reason, gin.H{"contracts": ids, "tasks": tasks}, auth.Principal(c).UserID).Scan(&id); err != nil {
			return nil, err
		}
		return gin.H{"id": id, "contracts": len(ids), "tasks": len(tasks)}, audit.Record(ctx, tx, "handover", "user", from, map[string]any{"to": to, "reason": in.Reason, "handover_id": id, "contracts": ids, "tasks": tasks})
	})
}
