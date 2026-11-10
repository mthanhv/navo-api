// Package crm keeps what sales learn in the field: notes about a customer (written on
// the phone, often offline) and the follow-up tasks they create (REQUIREMENTS 5.10),
// the people at each customer and supplier, and product-news emails to customer contacts.
package crm

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"navo-api/internal/apperrors"
	"navo-api/internal/audit"
	"navo-api/internal/auth"
	"navo-api/internal/db"
	"navo-api/internal/httpx"
	"navo-api/internal/mailer"
	"navo-api/internal/response"
)

// Handlers serve notes, tasks, partner contacts and product-news emails.
type Handlers struct {
	Pool *pgxpool.Pool
	// Mail sends product-news emails; nil or disabled keeps sending off.
	Mail mailer.Sender
}

// Task is something to do, usually by the person who wrote the note.
type Task struct {
	ID             uuid.UUID  `json:"id"`
	Title          string     `json:"title"`
	DueOn          *string    `json:"due_on"`
	CustomerID     *uuid.UUID `json:"customer_id"`
	CustomerName   *string    `json:"customer_name"`
	ContractID     *uuid.UUID `json:"contract_id"`
	ContractNumber *string    `json:"contract_number"`
	DoneAt         *time.Time `json:"done_at"`
}

// Note is a field note about a customer.
type Note struct {
	ID             uuid.UUID  `json:"id"`
	ClientID       uuid.UUID  `json:"client_id"`
	CustomerID     uuid.UUID  `json:"customer_id"`
	CustomerName   string     `json:"customer_name"`
	ContractID     *uuid.UUID `json:"contract_id"`
	ContractNumber *string    `json:"contract_number"`
	Body           string     `json:"body"`
	WrittenAt      time.Time  `json:"written_at"`
	Author         string     `json:"author"`
	Tasks          []Task     `json:"tasks"`
}

type noteInput struct {
	ClientID   string  `json:"client_id"`
	CustomerID string  `json:"customer_id"`
	ContractID *string `json:"contract_id"`
	Body       string  `json:"body"`
	WrittenAt  *string `json:"written_at"`
	Task       *struct {
		Title string  `json:"title"`
		DueOn *string `json:"due_on"`
	} `json:"task"`
}

// CreateNote handles POST /notes. Sending the same client_id again returns the note
// already stored, so a phone can retry an offline draft safely.
func (h *Handlers) CreateNote(c *gin.Context) {
	p := auth.Principal(c)
	var in noteInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	clientID := httpx.UUID(&errs, "client_id", in.ClientID)
	customer := httpx.UUID(&errs, "customer_id", in.CustomerID)
	var contract *uuid.UUID
	if in.ContractID != nil && *in.ContractID != "" {
		v := httpx.UUID(&errs, "contract_id", *in.ContractID)
		contract = &v
	}
	in.Body = strings.TrimSpace(in.Body)
	if in.Body == "" {
		errs.Add("body", "required", nil)
	} else if len([]rune(in.Body)) > 5000 {
		errs.Add("body", "too_long", map[string]any{"max": 5000})
	}
	written := time.Now()
	if in.WrittenAt != nil && *in.WrittenAt != "" {
		w, err := time.Parse(time.RFC3339, *in.WrittenAt)
		if err != nil || w.After(time.Now().Add(5*time.Minute)) {
			errs.Add("written_at", "date", nil)
		} else {
			written = w
		}
	}
	var taskDue *time.Time
	if in.Task != nil {
		in.Task.Title = strings.TrimSpace(in.Task.Title)
		if in.Task.Title == "" {
			errs.Add("task.title", "required", nil)
		} else if len([]rune(in.Task.Title)) > 300 {
			errs.Add("task.title", "too_long", map[string]any{"max": 300})
		}
		taskDue = httpx.Date(&errs, "task.due_on", in.Task.DueOn)
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	ctx := c.Request.Context()
	var existing uuid.UUID
	if err := h.Pool.QueryRow(ctx, `SELECT id FROM notes WHERE client_id = $1 AND author_id = $2`, clientID, p.UserID).Scan(&existing); err == nil {
		n, err := h.loadNote(ctx, h.Pool, existing)
		if err != nil {
			response.Fail(c, err)
			return
		}
		response.OK(c, n)
		return
	}
	var id uuid.UUID
	err := db.InTx(ctx, h.Pool, func(tx pgx.Tx) error {
		var isCustomer bool
		if err := tx.QueryRow(ctx, `SELECT is_customer FROM organizations WHERE id = $1`, customer).Scan(&isCustomer); err != nil || !isCustomer {
			return apperrors.Invalid("customer_id", "not_customer", nil)
		}
		if contract != nil {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM contracts WHERE id = $1 AND customer_id = $2 AND ($3::uuid IS NULL OR owner_id = $3))`,
				*contract, customer, p.OwnerFilter()).Scan(&ok); err != nil || !ok {
				return apperrors.Invalid("contract_id", "not_in_contract", nil)
			}
		}
		if err := tx.QueryRow(ctx, `INSERT INTO notes (client_id, customer_id, contract_id, body, written_at, author_id)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (client_id) DO NOTHING RETURNING id`, clientID, customer, contract, in.Body, written, p.UserID).Scan(&id); err != nil {
			if err == pgx.ErrNoRows {
				return apperrors.Rule("note_exists", "This note was already sent.", nil)
			}
			return err
		}
		if in.Task != nil {
			if _, err := tx.Exec(ctx, `INSERT INTO tasks (title, due_on, owner_id, customer_id, contract_id, note_id, created_by) VALUES ($1, $2, $3, $4, $5, $6, $3)`,
				in.Task.Title, taskDue, p.UserID, customer, contract, id); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, "create", "note", id, map[string]any{"customer_id": customer, "with_task": in.Task != nil})
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	n, err := h.loadNote(ctx, h.Pool, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	c.JSON(201, gin.H{"request_id": c.GetString("request_id"), "status": 201, "success": true, "data": n})
}

const noteSelect = `SELECT n.id, n.client_id, n.customer_id, o.name, n.contract_id, c.number, n.body, n.written_at, u.full_name
	FROM notes n JOIN organizations o ON o.id = n.customer_id JOIN users u ON u.id = n.author_id LEFT JOIN contracts c ON c.id = n.contract_id`

func (h *Handlers) loadNote(ctx context.Context, q db.Querier, id uuid.UUID) (*Note, error) {
	var n Note
	if err := q.QueryRow(ctx, noteSelect+` WHERE n.id = $1`, id).Scan(&n.ID, &n.ClientID, &n.CustomerID, &n.CustomerName, &n.ContractID, &n.ContractNumber, &n.Body, &n.WrittenAt, &n.Author); err != nil {
		return nil, err
	}
	tasks, err := h.tasks(ctx, q, `t.note_id = $1`, id)
	n.Tasks = tasks
	return &n, err
}

// ListNotes handles GET /notes?customer_id=&contract_id=: newest first, within the caller's scope.
func (h *Handlers) ListNotes(c *gin.Context) {
	ctx := c.Request.Context()
	p := auth.Principal(c)
	where, args := []string{`($1::uuid IS NULL OR n.author_id = $1 OR c.owner_id = $1)`}, []any{p.OwnerFilter()}
	for _, f := range []string{"customer_id", "contract_id"} {
		if v := c.Query(f); v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				response.Fail(c, apperrors.Invalid(f, "uuid", nil))
				return
			}
			args = append(args, id)
			where = append(where, "n."+f+" = $"+strconv.Itoa(len(args)))
		}
	}
	rows, err := h.Pool.Query(ctx, noteSelect+` WHERE `+strings.Join(where, " AND ")+` ORDER BY n.written_at DESC LIMIT 100`, args...)
	if err != nil {
		response.Fail(c, err)
		return
	}
	var ids []uuid.UUID
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.ClientID, &n.CustomerID, &n.CustomerName, &n.ContractID, &n.ContractNumber, &n.Body, &n.WrittenAt, &n.Author); err != nil {
			rows.Close()
			response.Fail(c, err)
			return
		}
		ids = append(ids, n.ID)
	}
	rows.Close()
	out := []*Note{}
	for _, id := range ids {
		n, err := h.loadNote(ctx, h.Pool, id)
		if err != nil {
			response.Fail(c, err)
			return
		}
		out = append(out, n)
	}
	response.OK(c, out)
}

func (h *Handlers) tasks(ctx context.Context, q db.Querier, where string, args ...any) ([]Task, error) {
	rows, err := q.Query(ctx, `SELECT t.id, t.title, t.due_on, t.customer_id, o.name, t.contract_id, c.number, t.done_at
		FROM tasks t LEFT JOIN organizations o ON o.id = t.customer_id LEFT JOIN contracts c ON c.id = t.contract_id
		WHERE `+where+` ORDER BY t.done_at IS NOT NULL, t.due_on NULLS LAST, t.created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		var t Task
		var due *time.Time
		if err := rows.Scan(&t.ID, &t.Title, &due, &t.CustomerID, &t.CustomerName, &t.ContractID, &t.ContractNumber, &t.DoneAt); err != nil {
			return nil, err
		}
		if due != nil {
			s := due.Format(time.DateOnly)
			t.DueOn = &s
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// MyTasks handles GET /tasks: the caller's open tasks, plus those finished today.
func (h *Handlers) MyTasks(c *gin.Context) {
	p := auth.Principal(c)
	out, err := h.tasks(c.Request.Context(), h.Pool, `t.owner_id = $1 AND (t.done_at IS NULL OR t.done_at > now() - interval '12 hours')`, p.UserID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, out)
}

type taskInput struct {
	Title      string  `json:"title"`
	DueOn      *string `json:"due_on"`
	CustomerID *string `json:"customer_id"`
	ContractID *string `json:"contract_id"`
}

// CreateTask handles POST /tasks (for the caller).
func (h *Handlers) CreateTask(c *gin.Context) {
	p := auth.Principal(c)
	var in taskInput
	if err := httpx.Decode(c, &in); err != nil {
		response.Fail(c, err)
		return
	}
	var errs apperrors.FieldErrors
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || len([]rune(in.Title)) > 300 {
		errs.Add("title", "required", nil)
	}
	due := httpx.Date(&errs, "due_on", in.DueOn)
	var customer, contract *uuid.UUID
	if in.CustomerID != nil && *in.CustomerID != "" {
		v := httpx.UUID(&errs, "customer_id", *in.CustomerID)
		customer = &v
	}
	if in.ContractID != nil && *in.ContractID != "" {
		v := httpx.UUID(&errs, "contract_id", *in.ContractID)
		contract = &v
	}
	if err := errs.Err(); err != nil {
		response.Fail(c, err)
		return
	}
	httpx.Command(c, h.Pool, "tasks.create", false, httpx.Created, func(tx pgx.Tx) (any, error) {
		ctx := c.Request.Context()
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO tasks (title, due_on, owner_id, customer_id, contract_id, created_by) VALUES ($1, $2, $3, $4, $5, $3) RETURNING id`,
			in.Title, due, p.UserID, customer, contract).Scan(&id); err != nil {
			return nil, err
		}
		ts, err := h.tasks(ctx, tx, `t.id = $1`, id)
		if err != nil || len(ts) == 0 {
			return nil, err
		}
		return ts[0], nil
	})
}

// SetTaskDone handles POST /tasks/:id/done and /tasks/:id/reopen (own tasks only).
func (h *Handlers) SetTaskDone(done bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		p := auth.Principal(c)
		id, err := httpx.PathID(c, "id")
		if err != nil {
			response.Fail(c, err)
			return
		}
		ctx := c.Request.Context()
		tag, err := h.Pool.Exec(ctx, `UPDATE tasks SET done_at = CASE WHEN $3 THEN coalesce(done_at, now()) END WHERE id = $1 AND owner_id = $2`, id, p.UserID, done)
		if err != nil {
			response.Fail(c, err)
			return
		}
		if tag.RowsAffected() == 0 {
			response.Fail(c, apperrors.NotFound())
			return
		}
		ts, err := h.tasks(ctx, h.Pool, `t.id = $1`, id)
		if err != nil || len(ts) == 0 {
			response.Fail(c, err)
			return
		}
		response.OK(c, ts[0])
	}
}
