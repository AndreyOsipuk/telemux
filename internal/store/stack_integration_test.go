//go:build integration

package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/stack"
)

// setupStack - все миграции и машины: релей pr1 (telemt и mtg), выход ps1 (только telemt),
// выход ps2 (агент не отчитывался), вход панели pan1.
func setupStack(t *testing.T) *Store {
	t.Helper()
	st := setupAll(t)
	seedTargets(t, st,
		telemtTarget("pr1", "relay"),
		Target{Code: "ps1", Kind: "exit", RelayCode: "pr1", Enabled: true, Resources: []string{ResourceTelemtUsers}},
		Target{Code: "ps2", Kind: "exit", RelayCode: "pr1", Enabled: true, Resources: []string{}},
		Target{Code: "pan1", Kind: "panel", Enabled: true, Resources: []string{ResourceXuiClients}})
	ctx := context.Background()
	for _, rep := range []RuntimeReport{
		{Target: "pr1", AgentVersion: "v1", Host: &RuntimeHost{CPUPct: 1},
			Telemt: &RuntimeTelemt{Version: "3.5.10", APIOK: true}, Mtg: &RuntimeMtg{Version: "1.15.0-webalias", APIOK: true}},
		{Target: "ps1", AgentVersion: "v1", Telemt: &RuntimeTelemt{Version: "", APIOK: false}},
	} {
		if err := st.RecordRuntime(ctx, rep); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func binOf(version, sha string) stack.Binary {
	return stack.Binary{Version: version, SHA256: strings.Repeat(sha, 64), Size: 100}
}

func setObserved(t *testing.T, st *Store, code string, rev int64, status, errText string) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(),
		`INSERT INTO observed_state (target, resource, revision, status, error, seen_at)
		 VALUES ($1, 'stack', $2, $3, NULLIF($4, ''), now())
		 ON CONFLICT (target, resource) DO UPDATE
		   SET revision = EXCLUDED.revision, status = EXCLUDED.status, error = EXCLUDED.error, seen_at = now()`,
		code, rev, status, errText); err != nil {
		t.Fatal(err)
	}
}

// Миграция 0013 идемпотентна; схема видна проверке.
func TestIntegration_Stack_Schema(t *testing.T) {
	st := setupStack(t)
	defer st.Close()
	setupAll(t).Close()
	if err := st.CheckStackSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Ошибка проверки не стирает прошлый релиз.
func TestIntegration_Stack_Releases(t *testing.T) {
	st := setupStack(t)
	defer st.Close()
	ctx := context.Background()
	if err := st.SaveStackRelease(ctx, StackReleaseCheck{Product: "mtg", Repo: "dolonet/mtg-multi", Err: "HTTP 403"}); err != nil {
		t.Fatal(err)
	}
	pub := time.Date(2026, 11, 8, 12, 8, 51, 0, time.UTC)
	if err := st.SaveStackRelease(ctx, StackReleaseCheck{Product: "telemt", Repo: "telemt/telemt",
		Version: "3.5.17", Tag: "3.5.17", PublishedAt: pub}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveStackRelease(ctx, StackReleaseCheck{Product: "telemt", Repo: "telemt/telemt", Err: "timeout"}); err != nil {
		t.Fatal(err)
	}
	rels, err := st.StackReleases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 2 || rels[0].Product != "mtg" || rels[0].Version != nil || *rels[0].Error != "HTTP 403" {
		t.Fatalf("mtg: %+v", rels)
	}
	tm := rels[1]
	if tm.Version == nil || *tm.Version != "3.5.17" || !tm.PublishedAt.Equal(pub) || tm.Error == nil || *tm.Error != "timeout" || tm.CheckedAt == nil {
		t.Fatalf("telemt после ошибки: %+v", tm)
	}
	if err := st.SaveStackRelease(ctx, StackReleaseCheck{Product: "xray", Repo: "a/b"}); err == nil {
		t.Fatal("неизвестный продукт")
	}
}

// Список: только релеи и выходы, продукт есть только если агент его прислал, страница.
func TestIntegration_Stack_Machines(t *testing.T) {
	st := setupStack(t)
	defer st.Close()
	ctx := context.Background()
	list, total, err := st.StackMachines(ctx, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(list) != 3 {
		t.Fatalf("машин %d из %d: %+v", len(list), total, list)
	}
	pr1, ps1, ps2 := list[0], list[1], list[2]
	if pr1.Code != "pr1" || pr1.Telemt == nil || *pr1.Telemt.Current != "3.5.10" || pr1.Mtg == nil ||
		*pr1.Mtg.Current != "1.15.0-webalias" || pr1.Telemt.Update != nil || pr1.Arch != "" {
		t.Fatalf("pr1: %+v %+v %+v", pr1, pr1.Telemt, pr1.Mtg)
	}
	// Версия пустая - current nil, но продукт есть.
	if ps1.Telemt == nil || ps1.Telemt.Current != nil || ps1.Mtg != nil {
		t.Fatalf("ps1: %+v %+v", ps1.Telemt, ps1.Mtg)
	}
	if ps2.Telemt != nil || ps2.Mtg != nil {
		t.Fatalf("ps2 без отчёта: %+v", ps2)
	}
	page, total, err := st.StackMachines(ctx, 1, 1)
	if err != nil || total != 3 || len(page) != 1 || page[0].Code != "ps1" {
		t.Fatalf("страница: %+v %d %v", page, total, err)
	}
	if _, err := st.StackMachine(ctx, "nobody"); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("нет машины: %v", err)
	}
	if m, err := st.StackMachine(ctx, "pan1"); err != nil || m.Kind != "panel" || m.Telemt != nil {
		t.Fatalf("панель: %+v %v", m, err)
	}
}

// Публикация: ревизия только с обновляемым продуктом, ресурс назначается, повтор во
// время обновления - ErrStackUpdateInProgress, итог по отчёту агента, повтор после итога -
// новая ревизия даже с тем же бинарём.
func TestIntegration_Stack_PublishAndFinish(t *testing.T) {
	st := setupStack(t)
	defer st.Close()
	ctx := context.Background()
	from := "3.5.10"
	u1, err := st.PublishStackUpdate(ctx, "pr1", "telemt", binOf("3.5.17", "a"), &from)
	if err != nil {
		t.Fatal(err)
	}
	if u1.Revision == 0 || u1.Status != "pending" || u1.RequestedAt.IsZero() {
		t.Fatalf("обновление: %+v", u1)
	}
	if _, err := st.PublishStackUpdate(ctx, "pr1", "telemt", binOf("3.5.18", "b"), &from); !errors.Is(err, ErrStackUpdateInProgress) {
		t.Fatalf("повтор во время обновления: %v", err)
	}
	c, _ := stackContent(t, st, "pr1")
	if len(c) != 1 || c["telemt"].Version != "3.5.17" {
		t.Fatalf("содержимое: %+v", c)
	}
	tg, err := st.TargetByCode(ctx, "pr1")
	if err != nil || !slices.Contains(tg.Resources, ResourceStack) || len(tg.Resources) != 2 {
		t.Fatalf("ресурсы машины: %+v %v", tg.Resources, err)
	}
	if b, err := st.DesiredBundle(ctx, "pr1"); err != nil || b.Revision != u1.Revision {
		t.Fatalf("бандл: %+v %v", b, err)
	}
	// Бинари - только этой машине и только из последней ревизии.
	for sha, want := range map[string]bool{strings.Repeat("a", 64): true, strings.Repeat("b", 64): false} {
		if ok, err := st.StackBlobAllowed(ctx, "pr1", sha); err != nil || ok != want {
			t.Fatalf("бинарь %s у pr1: %v %v", sha[:4], ok, err)
		}
	}
	if ok, err := st.StackBlobAllowed(ctx, "ps1", strings.Repeat("a", 64)); err != nil || ok {
		t.Fatalf("чужой бинарь: %v %v", ok, err)
	}

	// Отчёт агента о старой ревизии - ещё pending.
	setObserved(t, st, "pr1", u1.Revision-1, "applied", "")
	m, err := st.StackMachine(ctx, "pr1")
	if err != nil || m.Telemt.Update == nil || m.Telemt.Update.Status != "pending" || *m.Telemt.Update.From != "3.5.10" {
		t.Fatalf("до отчёта: %+v %v", m.Telemt.Update, err)
	}
	setObserved(t, st, "pr1", u1.Revision, "applied", "")
	m, _ = st.StackMachine(ctx, "pr1")
	if m.Telemt.Update.Status != "applied" || m.Telemt.Update.FinishedAt == nil {
		t.Fatalf("после отчёта: %+v", m.Telemt.Update)
	}
	done, err := st.FinishStackUpdates(ctx)
	if err != nil || len(done) != 1 || done[0].Product != "telemt" || done[0].Status != "applied" {
		t.Fatalf("итог: %+v %v", done, err)
	}

	mfrom := "1.15.0-webalias"
	u2, err := st.PublishStackUpdate(ctx, "pr1", "mtg", binOf("1.15.0", "c"), &mfrom)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := stackContent(t, st, "pr1"); len(c) != 1 || c["mtg"].Version != "1.15.0" {
		t.Fatalf("mtg: %+v", c)
	}
	// Записанный итог telemt не меняется от отчёта по ревизии mtg.
	setObserved(t, st, "pr1", u2.Revision, "failed", "mtg-multi не поднялся")
	m, _ = st.StackMachine(ctx, "pr1")
	if m.Telemt.Update.Status != "applied" || m.Mtg.Update.Status != "failed" || *m.Mtg.Update.Error != "mtg-multi не поднялся" {
		t.Fatalf("провал mtg: %+v %+v", m.Telemt.Update, m.Mtg.Update)
	}
	if done, err := st.FinishStackUpdates(ctx); err != nil || len(done) != 1 || done[0].Status != "failed" {
		t.Fatalf("итог провала: %+v %v", done, err)
	}
	// Повтор того же бинаря после провала - новая ревизия.
	u3, err := st.PublishStackUpdate(ctx, "pr1", "mtg", binOf("1.15.0", "c"), &mfrom)
	if err != nil || u3.Revision <= u2.Revision {
		t.Fatalf("повтор после провала: %+v %v", u3, err)
	}
	m, _ = st.StackMachine(ctx, "pr1")
	if m.Mtg.Update.Status != "pending" || m.Mtg.Update.Error != nil || m.Mtg.Update.FinishedAt != nil {
		t.Fatalf("новая попытка: %+v", m.Mtg.Update)
	}
	if c, rev := stackContent(t, st, "pr1"); rev != u3.Revision || c["mtg"].SHA256 != strings.Repeat("c", 64) {
		t.Fatalf("ревизия: %d %+v", rev, c)
	}
}

// Режим compare и молчание агента дольше таймаута - failed с причиной.
func TestIntegration_Stack_CompareAndTimeout(t *testing.T) {
	st := setupStack(t)
	defer st.Close()
	ctx := context.Background()
	u, err := st.PublishStackUpdate(ctx, "pr1", "telemt", binOf("3.5.17", "a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	setObserved(t, st, "pr1", u.Revision, "compared", "")
	m, _ := st.StackMachine(ctx, "pr1")
	if m.Telemt.Update.Status != "failed" || !strings.Contains(*m.Telemt.Update.Error, "compare") || m.Telemt.Update.From != nil {
		t.Fatalf("compare: %+v", m.Telemt.Update)
	}
	// Публикация сама записывает итог telemt.
	if _, err = st.PublishStackUpdate(ctx, "pr1", "mtg", binOf("1.15.0", "c"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE stack_updates SET requested_at = now() - interval '31 minutes' WHERE target = 'pr1' AND product = 'mtg'`); err != nil {
		t.Fatal(err)
	}
	m, _ = st.StackMachine(ctx, "pr1")
	if m.Telemt.Update.Status != "failed" || m.Mtg.Update.Status != "failed" || *m.Mtg.Update.Error != stackTimeoutError {
		t.Fatalf("таймаут: %+v %+v", m.Telemt.Update, m.Mtg.Update)
	}
	done, err := st.FinishStackUpdates(ctx)
	if err != nil || len(done) != 1 || done[0].Product != "mtg" {
		t.Fatalf("итог: %+v %v", done, err)
	}
	// После таймаута можно снова.
	if _, err := st.PublishStackUpdate(ctx, "pr1", "mtg", binOf("1.15.0", "c"), nil); err != nil {
		t.Fatal(err)
	}
}

// Выключенной и неизвестной машине не публикуется; удаление машины чистит обновления.
func TestIntegration_Stack_PublishGuards(t *testing.T) {
	st := setupStack(t)
	defer st.Close()
	ctx := context.Background()
	if _, err := st.PublishStackUpdate(ctx, "nobody", "telemt", binOf("3.5.17", "a"), nil); !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("нет машины: %v", err)
	}
	off := false
	if _, err := st.UpdateTarget(ctx, "ps1", TargetPatch{Enabled: &off}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PublishStackUpdate(ctx, "ps1", "telemt", binOf("3.5.17", "a"), nil); !errors.Is(err, ErrMachineDisabled) {
		t.Fatalf("выключена: %v", err)
	}
	if countRows(t, st, "stack_updates") != 0 {
		t.Fatal("отказ не должен писать обновление")
	}
	if _, err := st.PublishStackUpdate(ctx, "ps2", "telemt", binOf("3.5.17", "a"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateTarget(ctx, "ps2", TargetPatch{Enabled: &off}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteTarget(ctx, "ps2"); err != nil {
		t.Fatal(err)
	}
	if countRows(t, st, "stack_updates") != 0 {
		t.Fatal("обновления удалённой машины должны уйти каскадом")
	}
}

// stackContent - содержимое последней ревизии ресурса stack машины.
func stackContent(t *testing.T, st *Store, code string) (stack.Content, int64) {
	t.Helper()
	d, err := st.LatestDesired(context.Background(), code, ResourceStack)
	if err != nil {
		t.Fatal(err)
	}
	c, err := stack.Parse(d.Content)
	if err != nil {
		t.Fatalf("ресурс stack %q: %v", d.Content, err)
	}
	return c, d.Revision
}

// Ресурс stack - разовая команда: после итога (applied, failed, compare, таймаут) продукт
// уходит из ресурса новой ревизией, и агент при перепроверке его не применяет снова.
func TestIntegration_Stack_ProductLeavesResourceAfterFinish(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		finish func(t *testing.T, st *Store, rev int64)
		status string
	}{
		{"applied", func(t *testing.T, st *Store, rev int64) { setObserved(t, st, "pr1", rev, "applied", "") }, "applied"},
		{"failed", func(t *testing.T, st *Store, rev int64) {
			setObserved(t, st, "pr1", rev, "failed", "не поднялся")
		}, "failed"},
		{"compare", func(t *testing.T, st *Store, rev int64) { setObserved(t, st, "pr1", rev, "compared", "") }, "failed"},
		{"timeout", func(t *testing.T, st *Store, _ int64) {
			if _, err := st.pool.Exec(ctx, `UPDATE stack_updates SET requested_at = now() - interval '31 minutes'`); err != nil {
				t.Fatal(err)
			}
		}, "failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := setupStack(t)
			defer st.Close()
			u, err := st.PublishStackUpdate(ctx, "pr1", "mtg", binOf("1.16.0", "c"), nil)
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := stackContent(t, st, "pr1"); len(got) != 1 || got["mtg"].Version != "1.16.0" {
				t.Fatalf("до итога: %+v", got)
			}
			c.finish(t, st, u.Revision)
			done, err := st.FinishStackUpdates(ctx)
			if err != nil || len(done) != 1 || done[0].Status != c.status {
				t.Fatalf("итог: %+v %v", done, err)
			}
			got, rev := stackContent(t, st, "pr1")
			if len(got) != 0 || rev <= u.Revision {
				t.Fatalf("после итога продукт должен уйти новой ревизией: %+v, ревизия %d (была %d)", got, rev, u.Revision)
			}
			if ok, _ := st.StackBlobAllowed(ctx, "pr1", strings.Repeat("c", 64)); ok {
				t.Fatal("бинарь завершённого обновления больше не отдаётся")
			}
			// Повторный проход ничего не публикует.
			if _, err := st.FinishStackUpdates(ctx); err != nil {
				t.Fatal(err)
			}
			if _, rev2 := stackContent(t, st, "pr1"); rev2 != rev {
				t.Fatalf("лишняя ревизия: %d -> %d", rev, rev2)
			}
		})
	}
}

// На машине одно обновление за раз: пока идёт mtg, telemt - ErrStackUpdateInProgress;
// новое обновление публикует только свой продукт.
func TestIntegration_Stack_OneUpdatePerMachine(t *testing.T) {
	st := setupStack(t)
	defer st.Close()
	ctx := context.Background()
	u, err := st.PublishStackUpdate(ctx, "pr1", "mtg", binOf("1.16.0", "c"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PublishStackUpdate(ctx, "pr1", "telemt", binOf("3.5.17", "a"), nil); !errors.Is(err, ErrStackUpdateInProgress) {
		t.Fatalf("другой продукт при идущем обновлении: %v", err)
	}
	// Другой машине можно.
	if _, err := st.PublishStackUpdate(ctx, "ps1", "telemt", binOf("3.5.17", "a"), nil); err != nil {
		t.Fatal(err)
	}
	// Отчёт агента дошёл, но итог ещё не записан циклом - публикация запишет его сама.
	setObserved(t, st, "pr1", u.Revision, "applied", "")
	if _, err := st.PublishStackUpdate(ctx, "pr1", "telemt", binOf("3.5.17", "a"), nil); err != nil {
		t.Fatal(err)
	}
	got, _ := stackContent(t, st, "pr1")
	if len(got) != 1 || got["telemt"].Version != "3.5.17" {
		t.Fatalf("в ресурсе только обновляемый продукт: %+v", got)
	}
	m, _ := st.StackMachine(ctx, "pr1")
	if m.Mtg.Update.Status != "applied" || m.Mtg.Update.FinishedAt == nil || m.Telemt.Update.Status != "pending" {
		t.Fatalf("статусы: %+v %+v", m.Mtg.Update, m.Telemt.Update)
	}
}
