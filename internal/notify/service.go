package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"navo-api/internal/core"
	"navo-api/internal/progress"
)

// New builds the service from the configuration.
func New(pool *pgxpool.Pool, base, publicURL string, tz *time.Location) *Service {
	if tz == nil {
		tz = time.UTC
	}
	return &Service{Pool: pool, Zalo: Zalo{Base: base}, TZ: tz, PublicURL: publicURL}
}

// Service reads the bot's incoming messages, queues notifications and sends them.
type Service struct {
	Pool                 *pgxpool.Pool
	Zalo                 Zalo
	TZ                   *time.Location
	PublicURL            string // links in messages; empty leaves them out
	Now                  func() time.Time
	authorizationVersion int64
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().In(s.TZ)
	}
	return time.Now().In(s.TZ)
}

// Run works every interval until ctx ends.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("notifications", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

type settings struct {
	token                      *string
	digest, quietFrom, quietTo time.Duration
	lastDigest                 *time.Time
	offset                     int64
	authorizationVersion       int64
}

// RunOnce handles incoming messages, queues what is due, and sends what is pending.
func (s *Service) RunOnce(ctx context.Context) error {
	var st settings
	err := s.Pool.QueryRow(ctx, `SELECT zalo_token, extract(epoch FROM digest_time)::bigint, extract(epoch FROM quiet_from)::bigint, extract(epoch FROM quiet_to)::bigint, last_digest_on, update_offset, authorization_version
		FROM notification_settings`).Scan(&st.token, ptrSecs(&st.digest), ptrSecs(&st.quietFrom), ptrSecs(&st.quietTo), &st.lastDigest, &st.offset, &st.authorizationVersion)
	if err != nil {
		return err
	}
	if st.token == nil {
		return nil
	}
	// Each tick renders against one version. Never stamp old content with newer access.
	run := *s
	run.authorizationVersion = st.authorizationVersion
	s = &run
	token := *st.token
	if err := s.readUpdates(ctx, token, st.offset); err != nil {
		s.recordError(ctx, err)
	}
	if err := s.queueApprovals(ctx); err != nil {
		return err
	}
	now := s.now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	sinceMidnight := time.Duration(now.Hour())*time.Hour + time.Duration(now.Minute())*time.Minute
	if now.Weekday() != time.Sunday && sinceMidnight >= st.digest && (st.lastDigest == nil || st.lastDigest.Before(today)) {
		if err := s.queueDaily(ctx, today); err != nil {
			return err
		}
		if _, err := s.Pool.Exec(ctx, `UPDATE notification_settings SET last_digest_on = $1 WHERE authorization_version=$2`, today, s.authorizationVersion); err != nil {
			return err
		}
	}
	if quiet(sinceMidnight, st.quietFrom, st.quietTo) {
		return nil
	}
	return s.sendPending(ctx, token)
}

// ptrSecs scans a whole number of seconds into a duration.
func ptrSecs(d *time.Duration) any { return &secs{d} }

type secs struct{ d *time.Duration }

func (x *secs) Scan(src any) error {
	switch v := src.(type) {
	case int64:
		*x.d = time.Duration(v) * time.Second
	case nil:
		*x.d = 0
	default:
		return fmt.Errorf("seconds: %T", src)
	}
	return nil
}

// quiet reports whether t (since midnight) falls in quiet hours from→to, which may wrap midnight.
func quiet(t, from, to time.Duration) bool {
	if from == to {
		return false
	}
	if from < to {
		return t >= from && t < to
	}
	return t >= from || t < to
}

func (s *Service) recordError(ctx context.Context, err error) {
	_, _ = s.Pool.Exec(ctx, `UPDATE notification_settings SET last_error = $1, last_error_at = now()`, err.Error())
}

var linkCode = regexp.MustCompile(`\b(\d{6})\b`)

// readUpdates links people who send their code and registers groups the bot is @mentioned in.
func (s *Service) readUpdates(ctx context.Context, token string, offset int64) error {
	updates, err := s.Zalo.Updates(ctx, token, offset, 0)
	if err != nil {
		return err
	}
	next := offset
	for _, u := range updates {
		if u.ID >= next {
			next = u.ID + 1
		}
		if strings.EqualFold(u.ChatType, "PRIVATE") || u.ChatType == "" {
			s.handlePerson(ctx, token, u)
		} else {
			s.handleGroup(ctx, token, u)
		}
	}
	if next != offset {
		_, err = s.Pool.Exec(ctx, `UPDATE notification_settings SET update_offset = $1`, next)
	}
	return err
}

func (s *Service) handlePerson(ctx context.Context, token string, u Update) {
	m := linkCode.FindStringSubmatch(u.Text)
	if m == nil {
		var linked bool
		_ = s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM zalo_chats WHERE chat_id = $1)`, u.ChatID).Scan(&linked)
		if !linked {
			_ = s.Zalo.Send(ctx, token, u.ChatID, "Chào bạn! Để nhận thông báo Navo tại đây: mở Navo → Tài khoản → Kết nối Zalo, rồi gửi mã 6 số cho bot này.")
		}
		return
	}
	var userID uuid.UUID
	var name string
	err := s.Pool.QueryRow(ctx, `DELETE FROM zalo_link_codes c USING users u WHERE c.code = $1 AND c.expires_at > now() AND u.id = c.user_id AND u.is_active
		RETURNING u.id, u.full_name`, m[1]).Scan(&userID, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = s.Zalo.Send(ctx, token, u.ChatID, "Mã không đúng hoặc đã hết hạn. Lấy mã mới trong Navo → Tài khoản → Kết nối Zalo.")
		return
	}
	if err != nil {
		return
	}
	// One Zalo per person, one person per Zalo: relinking moves it.
	_, err = s.Pool.Exec(ctx, `WITH gone AS (DELETE FROM zalo_chats WHERE (user_id = $1 OR chat_id = $2) AND kind = 'person')
		INSERT INTO zalo_chats (chat_id, kind, title, user_id) VALUES ($2, 'person', $3, $1)`, userID, u.ChatID, name)
	if err != nil {
		slog.Warn("zalo link", "error", err)
		return
	}
	_ = s.Zalo.Send(ctx, token, u.ChatID, "✅ Đã kết nối Zalo với Navo cho "+name+". Thông báo của bạn sẽ gửi tới đây.")
}

func (s *Service) handleGroup(ctx context.Context, token string, u Update) {
	tag, err := s.Pool.Exec(ctx, `INSERT INTO zalo_chats (chat_id, kind, title) VALUES ($1, 'group', $2) ON CONFLICT (chat_id) DO NOTHING`,
		u.ChatID, "Nhóm Zalo "+lastN(u.ChatID, 4))
	if err == nil && tag.RowsAffected() == 1 {
		_ = s.Zalo.Send(ctx, token, u.ChatID, "✅ Nhóm đã đăng ký với Navo. Quản trị viên chọn thông báo gửi vào nhóm trong Navo → Cài đặt → Thông báo.")
	}
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// enqueue adds one message for a chat; the same dedupe key is never queued twice.
func (s *Service) enqueue(ctx context.Context, rule, key string, chat uuid.UUID, body string) error {
	if len([]rune(body)) > 2000 {
		body = string([]rune(body)[:1990]) + "…"
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO notifications (dedupe_key, rule, chat_id, body, authorization_version)
 SELECT $1,$2,$3,$4,$5 FROM notification_settings WHERE authorization_version=$5
 ON CONFLICT (dedupe_key) DO UPDATE SET body=EXCLUDED.body, authorization_version=EXCLUDED.authorization_version,
 status='pending', attempts=0, error=NULL WHERE notifications.status='failed' AND notifications.error='access_changed'`,
		key+":"+chat.String(), rule, chat, body, s.authorizationVersion)
	return err
}

type rule struct {
	enabled, toPeople bool
	group             *uuid.UUID
}

func (s *Service) rule(ctx context.Context, key string) (rule, error) {
	var r rule
	err := s.Pool.QueryRow(ctx, `SELECT r.enabled, r.to_people, g.id FROM notification_rules r LEFT JOIN zalo_chats g ON g.id = r.group_id AND g.kind = 'group' WHERE r.key = $1`, key).
		Scan(&r.enabled, &r.toPeople, &r.group)
	return r, err
}

func (s *Service) link(path string) string {
	if s.PublicURL == "" {
		return ""
	}
	return "\n" + strings.TrimRight(s.PublicURL, "/") + path
}

// queueApprovals: every pending over-quantity request, to the people who may decide it (not the requester).
func (s *Service) queueApprovals(ctx context.Context) error {
	r, err := s.rule(ctx, "override_requested")
	if err != nil || !r.enabled {
		return err
	}
	rows, err := s.Pool.Query(ctx, `SELECT q.id, q.requested_by, u.full_name, q.reason, k.number,
		COALESCE((SELECT number FROM purchase_orders WHERE id = q.document_id), (SELECT number FROM delivery_orders WHERE id = q.document_id), '')
		FROM quantity_override_requests q JOIN users u ON u.id = q.requested_by JOIN contracts k ON k.id = q.contract_id
		WHERE q.status = 'pending' AND q.requested_at > now() - interval '7 days'`)
	if err != nil {
		return err
	}
	type req struct {
		id, by           uuid.UUID
		who, reason, doc string
		contract         *string
	}
	var list []req
	for rows.Next() {
		var x req
		if err := rows.Scan(&x.id, &x.by, &x.who, &x.reason, &x.contract, &x.doc); err != nil {
			rows.Close()
			return err
		}
		list = append(list, x)
	}
	rows.Close()
	for _, x := range list {
		body := fmt.Sprintf("⚠️ Đề nghị vượt số lượng chờ duyệt\n%s (%s) · %s\nLý do: %s%s", x.doc, deref(x.contract), x.who, x.reason, s.link("/approvals"))
		if r.toPeople {
			chats, err := s.chats(ctx, `SELECT c.id FROM zalo_chats c JOIN users u ON u.id = c.user_id WHERE u.is_active AND u.id <> $1
				AND (EXISTS (SELECT 1 FROM user_roles WHERE user_id = u.id AND role = 'admin') OR EXISTS (SELECT 1 FROM user_grants WHERE user_id = u.id AND grant_code = 'quantity.override'))`, x.by)
			if err != nil {
				return err
			}
			for _, c := range chats {
				if err := s.enqueue(ctx, "override_requested", "override:"+x.id.String(), c, body); err != nil {
					return err
				}
			}
		}
		if r.group != nil {
			if err := s.enqueue(ctx, "override_requested", "override:"+x.id.String(), *r.group, body); err != nil {
				return err
			}
		}
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *Service) chats(ctx context.Context, sql string, args ...any) ([]uuid.UUID, error) {
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// FlagText words a line's flag in Vietnamese for Zalo messages.
var FlagText = map[string]string{
	"delivery_overdue": "trễ hạn giao", "no_plan": "chưa có kế hoạch giao", "stock_short": "thiếu hàng để giao",
	"receipt_overdue": "hàng NCC về trễ", "under_ordered": "chưa đặt mua đủ", "over_ordered": "đặt mua vượt", "over_delivered": "giao vượt",
}

var actionOrder = []string{"delivery_overdue", "receipt_overdue", "stock_short", "no_plan", "under_ordered", "over_delivered", "over_ordered"}

// attention keeps the lines whose most important flag needs action, most urgent first.
func attention(lines []core.LineRow) []core.LineRow {
	rank := map[string]int{}
	for i, c := range actionOrder {
		rank[c] = i
	}
	out := []core.LineRow{}
	for _, l := range lines {
		if len(l.Flags) > 0 {
			if _, ok := rank[l.Flags[0].Code]; ok && l.Flags[0].Severity != progress.Done {
				out = append(out, l)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Flags[0].Code] < rank[out[j].Flags[0].Code] })
	return out
}

func lineText(l core.LineRow) string {
	return fmt.Sprintf("• %s dòng %d · %s: %s", l.ContractNumber, l.LineNo, l.NameOnContract, FlagText[l.Flags[0].Code])
}

// queueDaily: the morning "needs attention" digest, tomorrow's deliveries and expiring quotations.
func (s *Service) queueDaily(ctx context.Context, today time.Time) error {
	day := today.Format(time.DateOnly)
	if r, err := s.rule(ctx, "attention_digest"); err != nil {
		return err
	} else if r.enabled {
		all, err := core.QueryLines(ctx, s.Pool, nil, nil, today)
		if err != nil {
			return err
		}
		need := attention(all)
		if r.toPeople {
			rows, err := s.Pool.Query(ctx, `SELECT c.id, u.id, u.full_name, EXISTS (SELECT 1 FROM user_roles WHERE user_id = u.id AND role IN ('delivery', 'accounting', 'admin', 'sales_manager'))
				FROM zalo_chats c JOIN users u ON u.id = c.user_id WHERE u.is_active`)
			if err != nil {
				return err
			}
			type person struct {
				chat, user uuid.UUID
				name       string
				all        bool
			}
			var people []person
			for rows.Next() {
				var p person
				if err := rows.Scan(&p.chat, &p.user, &p.name, &p.all); err != nil {
					rows.Close()
					return err
				}
				people = append(people, p)
			}
			rows.Close()
			for _, p := range people {
				mine := []core.LineRow{}
				for _, l := range need {
					if p.all || l.OwnerID == p.user {
						mine = append(mine, l)
					}
				}
				if len(mine) == 0 {
					continue
				}
				if err := s.enqueue(ctx, "attention_digest", "digest:"+day, p.chat, digestText("Chào "+p.name+", hôm nay có", mine, s.link("/"))); err != nil {
					return err
				}
			}
		}
		if r.group != nil && len(need) > 0 {
			if err := s.enqueue(ctx, "attention_digest", "digest:"+day, *r.group, digestText("Toàn công ty hôm nay có", need, s.link("/"))+byOwner(need)); err != nil {
				return err
			}
		}
	}
	tomorrow := today.AddDate(0, 0, 1).Format(time.DateOnly)
	if r, err := s.rule(ctx, "deliveries_tomorrow"); err != nil {
		return err
	} else if r.enabled {
		rows, err := s.Pool.Query(ctx, `SELECT d.number, k.number, cu.name, k.owner_id, d.firmness FROM delivery_orders d JOIN contracts k ON k.id = d.contract_id
			JOIN organizations cu ON cu.id = k.customer_id WHERE d.status = 'draft' AND d.planned_on = $1 ORDER BY d.number`, tomorrow)
		if err != nil {
			return err
		}
		type delivery struct {
			doc, contract, customer, firmness string
			owner                             uuid.UUID
		}
		var list []delivery
		for rows.Next() {
			var d delivery
			var contract *string
			if err := rows.Scan(&d.doc, &contract, &d.customer, &d.owner, &d.firmness); err != nil {
				rows.Close()
				return err
			}
			d.contract = deref(contract)
			list = append(list, d)
		}
		rows.Close()
		if len(list) > 0 {
			text := func(ds []delivery) string {
				var b strings.Builder
				fmt.Fprintf(&b, "🚚 Giao hàng ngày mai (%s): %d đợt", today.AddDate(0, 0, 1).Format("02/01"), len(ds))
				for i, d := range ds {
					if i == 10 {
						fmt.Fprintf(&b, "\n… và %d đợt khác", len(ds)-10)
						break
					}
					tent := ""
					if d.firmness == "tentative" {
						tent = " (chưa chốt với khách)"
					}
					fmt.Fprintf(&b, "\n• %s · %s · %s%s", d.doc, d.contract, d.customer, tent)
				}
				return b.String() + s.link("/deliveries")
			}
			if r.toPeople {
				rows, err := s.Pool.Query(ctx, `SELECT c.id, u.id, EXISTS (SELECT 1 FROM user_roles WHERE user_id = u.id AND role IN ('delivery', 'admin', 'sales_manager'))
					FROM zalo_chats c JOIN users u ON u.id = c.user_id WHERE u.is_active`)
				if err != nil {
					return err
				}
				type person struct {
					chat, user uuid.UUID
					all        bool
				}
				var people []person
				for rows.Next() {
					var p person
					if err := rows.Scan(&p.chat, &p.user, &p.all); err != nil {
						rows.Close()
						return err
					}
					people = append(people, p)
				}
				rows.Close()
				for _, p := range people {
					mine := []delivery{}
					for _, d := range list {
						if p.all || d.owner == p.user {
							mine = append(mine, d)
						}
					}
					if len(mine) > 0 {
						if err := s.enqueue(ctx, "deliveries_tomorrow", "deliveries:"+day, p.chat, text(mine)); err != nil {
							return err
						}
					}
				}
			}
			if r.group != nil {
				if err := s.enqueue(ctx, "deliveries_tomorrow", "deliveries:"+day, *r.group, text(list)); err != nil {
					return err
				}
			}
		}
	}
	if r, err := s.rule(ctx, "quotation_expiring"); err != nil {
		return err
	} else if r.enabled {
		rows, err := s.Pool.Query(ctx, `SELECT q.number, q.title, cu.name, q.valid_until, c.id FROM quotations q JOIN organizations cu ON cu.id = q.customer_id
			LEFT JOIN zalo_chats c ON c.user_id = q.owner_id
			WHERE q.status = 'sent' AND q.valid_until BETWEEN $1 AND $2 ORDER BY q.valid_until, q.number`, today, today.AddDate(0, 0, 3))
		if err != nil {
			return err
		}
		type quote struct {
			number, title, customer string
			until                   time.Time
			chat                    *uuid.UUID
		}
		var list []quote
		for rows.Next() {
			var q quote
			if err := rows.Scan(&q.number, &q.title, &q.customer, &q.until, &q.chat); err != nil {
				rows.Close()
				return err
			}
			list = append(list, q)
		}
		rows.Close()
		for _, q := range list {
			body := fmt.Sprintf("⏳ Báo giá %s sắp hết hiệu lực (%s)\n%s · %s\nGọi khách chốt hoặc gia hạn báo giá.%s", q.number, q.until.Format("02/01"), q.customer, q.title, s.link("/quotations"))
			if r.toPeople && q.chat != nil {
				if err := s.enqueue(ctx, "quotation_expiring", "quote:"+q.number+":"+day, *q.chat, body); err != nil {
					return err
				}
			}
			if r.group != nil {
				if err := s.enqueue(ctx, "quotation_expiring", "quote:"+q.number+":"+day, *r.group, body); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func digestText(intro string, lines []core.LineRow, link string) string {
	count := map[string]int{}
	for _, l := range lines {
		count[l.Flags[0].Code]++
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📋 %s %d dòng hàng cần xử lý:", intro, len(lines))
	for _, c := range actionOrder {
		if count[c] > 0 {
			fmt.Fprintf(&b, "\n– %d %s", count[c], FlagText[c])
		}
	}
	b.WriteString("\nGấp nhất:")
	for i, l := range lines {
		if i == 5 {
			break
		}
		b.WriteString("\n" + lineText(l))
	}
	return b.String() + link
}

func byOwner(lines []core.LineRow) string {
	count := map[string]int{}
	for _, l := range lines {
		count[l.OwnerName]++
	}
	names := make([]string, 0, len(count))
	for n := range count {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		return count[names[i]] > count[names[j]] || count[names[i]] == count[names[j]] && names[i] < names[j]
	})
	var b strings.Builder
	b.WriteString("\nTheo người phụ trách:")
	for i, n := range names {
		if i == 6 {
			break
		}
		fmt.Fprintf(&b, "\n– %s: %d", n, count[n])
	}
	return b.String()
}

// sendPending delivers queued messages; a failure is retried twice more, then marked failed.
func (s *Service) sendPending(ctx context.Context, token string) error {
	rows, err := s.Pool.Query(ctx, `SELECT n.id, c.chat_id, n.body FROM notifications n JOIN zalo_chats c ON c.id = n.chat_id
		WHERE n.status = 'pending' ORDER BY n.created_at LIMIT 50`)
	if err != nil {
		return err
	}
	type msg struct {
		id         uuid.UUID
		chat, body string
	}
	var list []msg
	for rows.Next() {
		var m msg
		if err := rows.Scan(&m.id, &m.chat, &m.body); err != nil {
			rows.Close()
			return err
		}
		list = append(list, m)
	}
	rows.Close()
	for _, m := range list {
		// Recheck after loading the batch too: an offboarding/handover can invalidate
		// messages while another network send in this batch is in flight.
		var eligible bool
		if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(
   SELECT 1 FROM notifications n JOIN zalo_chats c ON c.id=n.chat_id
   JOIN notification_rules r ON r.key=n.rule CROSS JOIN notification_settings st
   LEFT JOIN users u ON u.id=c.user_id
   WHERE n.id=$1 AND n.status='pending' AND n.authorization_version=st.authorization_version
   AND st.zalo_token=$2 AND r.enabled
   AND ((c.kind='person' AND u.is_active AND r.to_people) OR (c.kind='group' AND r.group_id=c.id)))`, m.id, token).Scan(&eligible); err != nil {
			return err
		}
		if !eligible {
			if _, err := s.Pool.Exec(ctx, `UPDATE notifications SET status='failed',error='access_changed' WHERE id=$1 AND status='pending'`, m.id); err != nil {
				return err
			}
			continue
		}
		if err := s.Zalo.Send(ctx, token, m.chat, m.body); err != nil {
			s.recordError(ctx, err)
			if _, err := s.Pool.Exec(ctx, `UPDATE notifications SET attempts = attempts + 1, error = $2, status = CASE WHEN attempts + 1 >= 3 THEN 'failed' ELSE 'pending' END WHERE id = $1`, m.id, err.Error()); err != nil {
				return err
			}
			continue
		}
		if _, err := s.Pool.Exec(ctx, `UPDATE notifications SET status = 'sent', sent_at = now(), attempts = attempts + 1, error = NULL WHERE id = $1`, m.id); err != nil {
			return err
		}
	}
	return nil
}
