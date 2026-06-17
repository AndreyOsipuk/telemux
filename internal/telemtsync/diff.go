// Package telemtsync — чистый diff-движок синхронизации пользователей telemt.
//
// Модель: источник истины (PG) даёт DesiredUser[], нода telemt отдаёт текущее
// состояние через GET /v1/users (RemoteUser[]). ComputeDiff считает список
// операций (SyncOp[]) для приведения ноды к desired. Функция детерминирована,
// без сети и времени-now → полностью покрывается unit-тестами.
//
// ВАЖНО про secret: GET /v1/users НЕ возвращает secret. Значит дрейф секрета по
// списку определить НЕЛЬЗЯ — diff сравнивает только наблюдаемое (expiration,
// max_tcp_conns, enabled). Secret выставляется один раз при create.
//
// Инварианты безопасности:
//   - управляем только юзерами с managed-префиксом (чужие/ручные не трогаем);
//   - порядок операций: create → patch → delete (создаём раньше сноса, чтобы
//     не упереться в telemt last_user_forbidden при полной замене).
package telemtsync

import (
	"strings"
	"time"
)

// ManagedPrefix — префикс управляемых нами юзеров по умолчанию.
const ManagedPrefix = "sub_"

// DesiredUser — желаемое состояние юзера (из источника истины).
type DesiredUser struct {
	Username          string
	Secret            string  // hex; в diff по списку не сверяется
	ExpirationRFC3339 *string // nil = без срока
	MaxTCPConns       *int    // nil = дефолт ноды
}

// RemoteUser — текущее состояние юзера на ноде (из GET /v1/users). Без secret.
type RemoteUser struct {
	Username          string
	Enabled           bool
	ExpirationRFC3339 *string
	MaxTCPConns       *int
}

// OpKind — вид операции синхронизации.
type OpKind string

const (
	OpCreate OpKind = "create"
	OpPatch  OpKind = "patch"
	OpDelete OpKind = "delete"
)

// PatchFields — только реально изменённые поля. Set-флаги отличают «выставить
// значение» (в т.ч. в null) от «не менять».
type PatchFields struct {
	SetExpiration     bool
	ExpirationRFC3339 *string
	SetMaxTCPConns    bool
	MaxTCPConns       *int
	Enabled           *bool // nil = не менять
}

// SyncOp — одна операция. enable/disable моделируются как Patch{Enabled};
// маппинг на отдельные ручки telemt делает api-client.
type SyncOp struct {
	Kind     OpKind
	Username string

	// Поля create:
	Secret            string
	ExpirationRFC3339 *string
	MaxTCPConns       *int

	// Поля patch:
	Fields PatchFields
}

// Options — настройки diff.
type Options struct {
	ManagedPrefix string // пусто → ManagedPrefix
}

func (o Options) prefix() string {
	if o.ManagedPrefix == "" {
		return ManagedPrefix
	}
	return o.ManagedPrefix
}

// ExpirationEquals сравнивает RFC3339-таймстемпы по моменту времени (Z и +00:00,
// разные TZ — это один момент, не дрейф). nil = «без срока». Невалидную дату
// сравнивает как строку.
func ExpirationEquals(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if *a == *b {
		return true
	}
	ta, ea := time.Parse(time.RFC3339, *a)
	tb, eb := time.Parse(time.RFC3339, *b)
	if ea != nil || eb != nil {
		return *a == *b
	}
	return ta.Equal(tb)
}

func intEqual(a, b *int) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func patchFieldsFor(d DesiredUser, r RemoteUser) (PatchFields, bool) {
	var f PatchFields
	changed := false
	if !ExpirationEquals(d.ExpirationRFC3339, r.ExpirationRFC3339) {
		f.SetExpiration = true
		f.ExpirationRFC3339 = d.ExpirationRFC3339
		changed = true
	}
	if !intEqual(d.MaxTCPConns, r.MaxTCPConns) {
		f.SetMaxTCPConns = true
		f.MaxTCPConns = d.MaxTCPConns
		changed = true
	}
	// Активная подписка всегда enabled. Disable из diff не эмитим — снятие
	// юзера выражается delete-операцией.
	if !r.Enabled {
		t := true
		f.Enabled = &t
		changed = true
	}
	return f, changed
}

// ComputeDiff возвращает упорядоченный список операций (create→patch→delete).
// Идемпотентна: при desired == remote → пустой список.
func ComputeDiff(desired []DesiredUser, remote []RemoteUser, opts Options) []SyncOp {
	prefix := opts.prefix()

	remoteByName := make(map[string]RemoteUser, len(remote))
	for _, r := range remote {
		remoteByName[r.Username] = r
	}
	desiredByName := make(map[string]struct{}, len(desired))
	for _, d := range desired {
		desiredByName[d.Username] = struct{}{}
	}

	var creates, patches, deletes []SyncOp

	for _, d := range desired {
		r, ok := remoteByName[d.Username]
		if !ok {
			creates = append(creates, SyncOp{
				Kind:              OpCreate,
				Username:          d.Username,
				Secret:            d.Secret,
				ExpirationRFC3339: d.ExpirationRFC3339,
				MaxTCPConns:       d.MaxTCPConns,
			})
			continue
		}
		if f, changed := patchFieldsFor(d, r); changed {
			patches = append(patches, SyncOp{Kind: OpPatch, Username: d.Username, Fields: f})
		}
	}

	for _, r := range remote {
		if !strings.HasPrefix(r.Username, prefix) {
			continue // чужих не трогаем
		}
		if _, ok := desiredByName[r.Username]; !ok {
			deletes = append(deletes, SyncOp{Kind: OpDelete, Username: r.Username})
		}
	}

	out := make([]SyncOp, 0, len(creates)+len(patches)+len(deletes))
	out = append(out, creates...)
	out = append(out, patches...)
	out = append(out, deletes...)
	return out
}

// DiffSafety — метрики «массовости» удаления для guard от катастрофы.
type DiffSafety struct {
	TotalRemoteManaged int
	DeleteCount        int
	DeleteFraction     float64 // 0..1; 0 если на ноде нет управляемых
}

// ComputeSafety считает долю удаления от управляемых юзеров на ноде.
func ComputeSafety(ops []SyncOp, remote []RemoteUser, opts Options) DiffSafety {
	prefix := opts.prefix()
	total := 0
	for _, r := range remote {
		if strings.HasPrefix(r.Username, prefix) {
			total++
		}
	}
	del := 0
	for _, o := range ops {
		if o.Kind == OpDelete {
			del++
		}
	}
	frac := 0.0
	if total > 0 {
		frac = float64(del) / float64(total)
	}
	return DiffSafety{TotalRemoteManaged: total, DeleteCount: del, DeleteFraction: frac}
}

// MassDeleteOptions — параметры guard'а от массового сноса.
type MassDeleteOptions struct {
	Options
	FractionThreshold float64 // 0 → 0.5
	MinDeletes        int     // 0 → 5
}

// IsMassDelete → true, если diff выглядит как катастрофический снос (нужен force).
// Аналог SHRINK_THRESHOLD из файлового пути: защита от пустого/битого remote.
func IsMassDelete(ops []SyncOp, remote []RemoteUser, opts MassDeleteOptions) bool {
	ft := opts.FractionThreshold
	if ft == 0 {
		ft = 0.5
	}
	md := opts.MinDeletes
	if md == 0 {
		md = 5
	}
	s := ComputeSafety(ops, remote, opts.Options)
	return s.DeleteCount >= md && s.DeleteFraction > ft
}
