package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/stack"
	"github.com/AndreyOsipuk/telemux/internal/stackrel"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

// fakeStack - StackStore в памяти.
type fakeStack struct {
	mu        sync.Mutex
	releases  map[string]store.StackRelease
	machines  map[string]store.StackMachine
	content   map[string]stack.Content
	published []store.StackUpdate
	rev       int64
	failSave  error
	failRels  error
	finished  int
}

func newFakeStack() *fakeStack {
	return &fakeStack{
		releases: map[string]store.StackRelease{},
		machines: map[string]store.StackMachine{
			// Релей: telemt старше релиза, своя сборка mtg.
			"pr2": {Code: "pr2", Kind: "relay", Enabled: true,
				Telemt: &store.StackProductInfo{Current: strp("3.5.10")},
				Mtg:    &store.StackProductInfo{Current: strp("1.15.0-webalias")}},
			// Выход: telemt уже последний, mtg нет.
			"ps2": {Code: "ps2", Kind: "exit", Enabled: true,
				Telemt: &store.StackProductInfo{Current: strp("3.5.17")}},
			// Выключенный выход.
			"ps3": {Code: "ps3", Kind: "exit", Enabled: false,
				Telemt: &store.StackProductInfo{Current: strp("3.5.10")}},
			// Обновление telemt ещё идёт.
			"ps5": {Code: "ps5", Kind: "exit", Enabled: true,
				Mtg: &store.StackProductInfo{Current: strp("1.14.0")},
				Telemt: &store.StackProductInfo{Current: strp("3.5.10"), Update: &store.StackUpdate{
					Code: "ps5", Product: "telemt", From: strp("3.5.10"), To: "3.5.17", Revision: 7,
					Status: "pending", RequestedAt: time.Now().UTC()}}},
			// Машина на неизвестной архитектуре.
			"ps6": {Code: "ps6", Kind: "exit", Enabled: true, Arch: "riscv64",
				Telemt: &store.StackProductInfo{Current: strp("3.5.10")}},
		},
		content: map[string]stack.Content{},
		rev:     100,
	}
}

func (f *fakeStack) StackReleases(context.Context) ([]store.StackRelease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRels != nil {
		return nil, f.failRels
	}
	out := []store.StackRelease{}
	for _, p := range stack.Products {
		if r, ok := f.releases[p]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStack) SaveStackRelease(_ context.Context, c store.StackReleaseCheck) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSave != nil {
		return f.failSave
	}
	now := time.Now().UTC()
	r := f.releases[c.Product]
	r.Product, r.Repo, r.CheckedAt = c.Product, c.Repo, &now
	if c.Err != "" {
		r.Error = strp(c.Err)
	} else {
		pub := c.PublishedAt
		r.Version, r.Tag, r.PublishedAt, r.Error = strp(c.Version), strp(c.Tag), &pub, nil
	}
	f.releases[c.Product] = r
	return nil
}

func (f *fakeStack) StackMachines(_ context.Context, limit, offset int) ([]store.StackMachine, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	codes := []string{}
	for c := range f.machines {
		codes = append(codes, c)
	}
	slices.Sort(codes)
	out := []store.StackMachine{}
	for i, c := range codes {
		if i >= offset && len(out) < limit {
			out = append(out, f.machines[c])
		}
	}
	return out, len(codes), nil
}

func (f *fakeStack) StackMachine(_ context.Context, code string) (store.StackMachine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[code]
	if !ok {
		return m, store.ErrUnknownTarget
	}
	return m, nil
}

func (f *fakeStack) PublishStackUpdate(_ context.Context, code, product string, bin stack.Binary, from *string) (store.StackUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[code]
	if !ok {
		return store.StackUpdate{}, store.ErrUnknownTarget
	}
	f.rev++
	u := store.StackUpdate{Code: code, Product: product, From: from, To: bin.Version, Revision: f.rev,
		Status: "pending", RequestedAt: time.Now().UTC()}
	f.content[code] = stack.Content{product: bin}
	info := m.Telemt
	if product == stack.ProductMtg {
		info = m.Mtg
	}
	info.Update = &u
	f.published = append(f.published, u)
	return u, nil
}

func (f *fakeStack) StackBlobAllowed(_ context.Context, target, sha string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.content[target] {
		if b.SHA256 == sha {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStack) FinishStackUpdates(context.Context) ([]store.StackUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished++
	return nil, nil
}

// fakeReleases - релизы GitHub в памяти.
type fakeReleases struct {
	mu        sync.Mutex
	latest    map[string]stackrel.Release
	latestErr error
	binErr    error
	bins      map[string][]byte // тег -> бинарь
	latestN   int
	binN      int           // сколько раз скачивали
	binDelay  time.Duration // задержка скачивания (параллельные запросы)
	arch      string
}

func newFakeReleases() *fakeReleases {
	pub := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	return &fakeReleases{
		latest: map[string]stackrel.Release{
			"telemt": {Product: "telemt", Repo: "telemt/telemt", Tag: "3.5.17", Version: "3.5.17", PublishedAt: pub},
			"mtg":    {Product: "mtg", Repo: "dolonet/mtg-multi", Tag: "v1.15.0", Version: "1.15.0", PublishedAt: pub},
		},
		bins: map[string][]byte{"3.5.17": []byte("telemt-3.5.17-binary"), "3.5.9": []byte("telemt-3.5.9-binary"),
			"v1.15.0": []byte("mtg-1.15.0-binary")},
	}
}

func (f *fakeReleases) Repo(product string) string {
	if product == "mtg" {
		return "dolonet/mtg-multi"
	}
	return "telemt/telemt"
}

func (f *fakeReleases) Latest(_ context.Context, product string) (stackrel.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latestN++
	if f.latestErr != nil {
		return stackrel.Release{}, f.latestErr
	}
	return f.latest[product], nil
}

func (f *fakeReleases) ByVersion(_ context.Context, product, version string) (stackrel.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l := f.latest[product]; l.Tag == version || l.Version == version {
		return l, nil
	}
	if product == "telemt" && version == "3.5.9" {
		return stackrel.Release{Product: "telemt", Repo: "telemt/telemt", Tag: "3.5.9", Version: "3.5.9"}, nil
	}
	return stackrel.Release{}, stackrel.ErrReleaseNotFound
}

func (f *fakeReleases) Binary(_ context.Context, rel stackrel.Release, arch string) ([]byte, error) {
	f.mu.Lock()
	f.arch = arch
	f.binN++
	delay, err := f.binDelay, f.binErr
	data := f.bins[rel.Tag]
	f.mu.Unlock()
	time.Sleep(delay)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func stackServer(t *testing.T, inRecovery bool) (*Server, *fakeStack, *fakeReleases) {
	t.Helper()
	s, _, _ := agentServer(inRecovery)
	fs, fr := newFakeStack(), newFakeReleases()
	s.deps.Stack = &StackDeps{Store: fs, Releases: fr, Cache: stackrel.Cache{Dir: t.TempDir()}}
	s.routesStack()
	return s, fs, fr
}

// GET /api/v1/stack: продукты всегда все, оценка версий по машинам, страница.
func TestStack_Status(t *testing.T) {
	s, fs, _ := stackServer(t, false)
	rec := do(s, "GET", "/api/v1/stack", botToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ждали 200, получили %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Products []stackProduct `json:"products"`
		Data     []stackMachine `json:"data"`
		Paging   struct{ Total, Limit, Offset int }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Products) != 2 || got.Products[0].Latest != nil || got.Products[1].Repo != "dolonet/mtg-multi" {
		t.Fatalf("релизы до проверки: %+v", got.Products)
	}
	if got.Paging.Total != 5 || got.Paging.Limit != 20 || len(got.Data) != 5 {
		t.Fatalf("страница: %+v, машин %d", got.Paging, len(got.Data))
	}
	// Релиз не узнан - обновления нет, своя сборка видна.
	if pr2 := got.Data[0]; pr2.Code != "pr2" || pr2.Telemt.UpdateAvailable || pr2.Telemt.Latest != nil || !pr2.Mtg.CustomBuild {
		t.Fatalf("pr2 до проверки: %+v %+v", pr2.Telemt, pr2.Mtg)
	}

	if err := s.checkStackReleases(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec = do(s, "GET", "/api/v1/stack?limit=2", botToken, "")
	got.Data = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Products[0].Latest == nil || got.Products[0].Latest.Version != "3.5.17" || got.Products[1].Latest.Tag != "v1.15.0" {
		t.Fatalf("релизы после проверки: %+v", got.Products)
	}
	if len(got.Data) != 2 || got.Paging.Total != 5 || got.Paging.Limit != 2 {
		t.Fatalf("limit=2: %d машин, %+v", len(got.Data), got.Paging)
	}
	pr2, ps2 := got.Data[0], got.Data[1]
	if !pr2.Telemt.UpdateAvailable || pr2.Telemt.CustomBuild || *pr2.Telemt.Latest != "3.5.17" {
		t.Fatalf("pr2 telemt: %+v", pr2.Telemt)
	}
	// 1.15.0-webalias при релизе 1.15.0: своя сборка, не обновление.
	if pr2.Mtg.UpdateAvailable || !pr2.Mtg.CustomBuild {
		t.Fatalf("pr2 mtg: %+v", pr2.Mtg)
	}
	if ps2.Telemt.UpdateAvailable || ps2.Mtg != nil {
		t.Fatalf("ps2: %+v %+v", ps2.Telemt, ps2.Mtg)
	}

	if rec := do(s, "GET", "/api/v1/stack?limit=101", botToken, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=101: ждали 400, получили %d", rec.Code)
	}
	if rec := do(s, "GET", "/api/v1/stack", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена: ждали 401, получили %d", rec.Code)
	}
	_ = fs
}

// На реплике машин нет (data пустой), релизы видны.
func TestStack_StatusOnReplica(t *testing.T) {
	s, _, _ := stackServer(t, true)
	rec := do(s, "GET", "/api/v1/stack", botToken, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"data":[]`) ||
		!strings.Contains(rec.Body.String(), `"total":0`) {
		t.Fatalf("реплика: %d %s", rec.Code, rec.Body.String())
	}
}

// POST /api/v1/stack/check: ошибка GitHub - в error продукта, не 5xx; второй раз за
// минуту - 429; на реплике - 409; сбой базы - 5xx.
func TestStack_Check(t *testing.T) {
	s, fs, fr := stackServer(t, false)
	fr.latestErr = errors.New("github: HTTP 403 rate limit")
	rec := do(s, "POST", "/api/v1/stack/check", botToken, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "rate limit") {
		t.Fatalf("ошибка GitHub: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(s, "POST", "/api/v1/stack/check", botToken, "")
	if rec.Code != http.StatusTooManyRequests || errCode(t, rec.Body.Bytes()) != "too_many_requests" ||
		rec.Header().Get("Retry-After") == "" {
		t.Fatalf("второй раз за минуту: %d %s", rec.Code, rec.Body.String())
	}
	if fr.latestN != 2 {
		t.Fatalf("429 не должен ходить в GitHub: запросов %d", fr.latestN)
	}

	// Прошлый релиз остаётся при ошибке.
	s.stackSt.lastManual = time.Time{}
	fr.latestErr = nil
	if rec := do(s, "POST", "/api/v1/stack/check", botToken, ""); rec.Code != http.StatusOK {
		t.Fatalf("проверка: %d", rec.Code)
	}
	s.stackSt.lastManual = time.Time{}
	fr.latestErr = errors.New("timeout")
	rec = do(s, "POST", "/api/v1/stack/check", botToken, "")
	var body struct{ Products []stackProduct }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if p := body.Products[0]; p.Latest == nil || p.Latest.Version != "3.5.17" || p.Error == nil || *p.Error != "timeout" {
		t.Fatalf("после ошибки прошлый релиз должен остаться: %+v", p)
	}

	s.stackSt.lastManual = time.Time{}
	fs.failSave = errors.New("boom")
	if rec := do(s, "POST", "/api/v1/stack/check", botToken, ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("сбой базы: ждали 500, получили %d", rec.Code)
	}

	replica, _, _ := stackServer(t, true)
	if rec := do(replica, "POST", "/api/v1/stack/check", botToken, ""); rec.Code != http.StatusConflict {
		t.Fatalf("реплика: ждали 409, получили %d", rec.Code)
	}
}

// Каждый плохой ввод и каждое состояние машины - свой 4xx и код; релиз не скачался - 503.
func TestStack_UpdateErrors(t *testing.T) {
	s, fs, fr := stackServer(t, false)
	cases := []struct {
		name, path, body string
		status           int
		code             string
	}{
		{"мусор", "/api/v1/machines/pr2/update", `{`, 400, "bad_request"},
		{"лишнее поле", "/api/v1/machines/pr2/update", `{"product":"telemt","x":1}`, 400, "bad_request"},
		{"нет продукта", "/api/v1/machines/pr2/update", `{}`, 400, "unknown_product"},
		{"чужой продукт", "/api/v1/machines/pr2/update", `{"product":"xray"}`, 400, "unknown_product"},
		{"версия не по формату", "/api/v1/machines/pr2/update", `{"product":"telemt","version":"../x"}`, 400, "unknown_version"},
		{"нет релиза", "/api/v1/machines/pr2/update", `{"product":"telemt","version":"9.9.9"}`, 400, "unknown_version"},
		{"нет машины", "/api/v1/machines/nobody/update", `{"product":"telemt"}`, 404, "not_found"},
		{"код не по формату", "/api/v1/machines/X_Y/update", `{"product":"telemt"}`, 404, "not_found"},
		{"выключена", "/api/v1/machines/ps3/update", `{"product":"telemt"}`, 409, "machine_disabled"},
		{"продукта нет", "/api/v1/machines/ps2/update", `{"product":"mtg"}`, 409, "product_missing"},
		{"своя сборка", "/api/v1/machines/pr2/update", `{"product":"mtg"}`, 409, "custom_build"},
		{"не новее", "/api/v1/machines/ps2/update", `{"product":"telemt"}`, 409, "not_newer"},
		{"старее", "/api/v1/machines/pr2/update", `{"product":"telemt","version":"3.5.9"}`, 409, "not_newer"},
		{"идёт обновление", "/api/v1/machines/ps5/update", `{"product":"telemt","force":true}`, 409, "update_in_progress"},
		{"идёт обновление другого продукта", "/api/v1/machines/ps5/update", `{"product":"mtg","force":true}`, 409, "update_in_progress"},
		{"архитектура", "/api/v1/machines/ps6/update", `{"product":"telemt"}`, 409, "asset_missing"},
	}
	for _, c := range cases {
		rec := do(s, "POST", c.path, botToken, c.body)
		if rec.Code != c.status || errCode(t, rec.Body.Bytes()) != c.code {
			t.Fatalf("%s: ждали %d %s, получили %d %s", c.name, c.status, c.code, rec.Code, rec.Body.String())
		}
	}
	if len(fs.published) != 0 {
		t.Fatalf("ошибки не должны публиковать: %+v", fs.published)
	}

	fr.binErr = fmt.Errorf("telemt/telemt 3.5.17: %w telemt-x86_64-linux-musl.tar.gz", stackrel.ErrAssetMissing)
	rec := do(s, "POST", "/api/v1/machines/pr2/update", botToken, `{"product":"telemt"}`)
	if rec.Code != http.StatusConflict || errCode(t, rec.Body.Bytes()) != "asset_missing" {
		t.Fatalf("нет файла под архитектуру: %d %s", rec.Code, rec.Body.String())
	}

	fr.binErr = errors.New("SHA256 не сошлась")
	rec = do(s, "POST", "/api/v1/machines/pr2/update", botToken, `{"product":"telemt"}`)
	if rec.Code != http.StatusServiceUnavailable || errCode(t, rec.Body.Bytes()) != "release_unavailable" ||
		strings.Contains(rec.Body.String(), "SHA256") {
		t.Fatalf("сумма не сошлась: ждали 503 release_unavailable без деталей, получили %d %s", rec.Code, rec.Body.String())
	}
	if len(fs.published) != 0 {
		t.Fatal("503 не должен публиковать")
	}

	// Сбой базы при выборе релиза - не release_unavailable.
	fr.binErr = nil
	fs.failRels = errors.New("conn reset")
	rec = do(s, "POST", "/api/v1/machines/pr2/update", botToken, `{"product":"telemt"}`)
	if rec.Code != http.StatusInternalServerError || errCode(t, rec.Body.Bytes()) == "release_unavailable" {
		t.Fatalf("сбой базы: %d %s", rec.Code, rec.Body.String())
	}
	fs.failRels = nil

	replica, _, _ := stackServer(t, true)
	if rec := do(replica, "POST", "/api/v1/machines/pr2/update", botToken, `{"product":"telemt"}`); rec.Code != http.StatusConflict ||
		errCode(t, rec.Body.Bytes()) != "not_master" {
		t.Fatalf("реплика: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(s, "POST", "/api/v1/machines/pr2/update", "", `{"product":"telemt"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена: %d", rec.Code)
	}
}

// Обновление: релиз узнаётся, бинарь в кэше, ресурс опубликован, 202; повтор - 409;
// бинарь отдаётся только своей машине.
func TestStack_UpdateAndBlob(t *testing.T) {
	s, fs, fr := stackServer(t, false)
	rec := do(s, "POST", "/api/v1/machines/pr2/update", botToken, `{"product":"telemt"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("обновление: %d %s", rec.Code, rec.Body.String())
	}
	var u store.StackUpdate
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	if u.Code != "pr2" || u.To != "3.5.17" || u.From == nil || *u.From != "3.5.10" || u.Status != "pending" || u.Revision == 0 {
		t.Fatalf("ответ: %+v", u)
	}
	if fr.arch != "amd64" {
		t.Fatalf("архитектура по умолчанию - amd64, получили %q", fr.arch)
	}
	// Последний релиз не был известен - узнан и сохранён.
	if fs.releases["telemt"].Version == nil || *fs.releases["telemt"].Version != "3.5.17" {
		t.Fatalf("релиз не сохранён: %+v", fs.releases["telemt"])
	}
	bin := fs.content["pr2"]["telemt"]
	want := []byte("telemt-3.5.17-binary")
	if bin.Version != "3.5.17" || bin.SHA256 != stackrel.SHA256Hex(want) || bin.Size != int64(len(want)) {
		t.Fatalf("ресурс stack: %+v", bin)
	}
	cached, err := os.ReadFile(filepath.Join(s.deps.Stack.Cache.Dir, bin.SHA256))
	if err != nil || string(cached) != string(want) {
		t.Fatalf("кэш: %v %q", err, cached)
	}
	// Повтор, пока идёт, - 409 даже с force.
	rec = do(s, "POST", "/api/v1/machines/pr2/update", botToken, `{"product":"telemt","force":true}`)
	if rec.Code != http.StatusConflict || errCode(t, rec.Body.Bytes()) != "update_in_progress" {
		t.Fatalf("повтор: %d %s", rec.Code, rec.Body.String())
	}
	// Другой продукт, пока идёт telemt, - тоже 409: одно обновление на машину.
	rec = do(s, "POST", "/api/v1/machines/pr2/update", botToken, `{"product":"mtg","version":"1.15.0","force":true}`)
	if rec.Code != http.StatusConflict || errCode(t, rec.Body.Bytes()) != "update_in_progress" {
		t.Fatalf("mtg при идущем telemt: %d %s", rec.Code, rec.Body.String())
	}

	// Бинарь: своя машина - 200 с длиной; чужая машина, чужой sha, мусор - 4xx.
	path := "/v1/stack/blobs/" + bin.SHA256 + "?target=pr2"
	rec = do(s, "GET", path, pr2Token, "")
	if rec.Code != http.StatusOK || rec.Body.String() != string(want) ||
		rec.Header().Get("Content-Type") != "application/octet-stream" || rec.Header().Get("Content-Length") != "20" {
		t.Fatalf("бинарь: %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
	if rec := do(s, "GET", "/v1/stack/blobs/"+bin.SHA256+"?target=ps2", "token-ps2", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("чужой бинарь: ждали 404, получили %d", rec.Code)
	}
	if rec := do(s, "GET", "/v1/stack/blobs/"+bin.SHA256+"?target=ps2", pr2Token, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("чужая машина в target: ждали 403, получили %d", rec.Code)
	}
	if rec := do(s, "GET", "/v1/stack/blobs/"+strings.Repeat("a", 64)+"?target=pr2", pr2Token, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("неизвестный sha: ждали 404, получили %d", rec.Code)
	}
	if rec := do(s, "GET", "/v1/stack/blobs/xyz?target=pr2", pr2Token, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("sha не по формату: ждали 400, получили %d", rec.Code)
	}
	if rec := do(s, "GET", path, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена: ждали 401, получили %d", rec.Code)
	}
	// Опубликован, но кэш потерян - 404, а не 5xx.
	if err := os.Remove(filepath.Join(s.deps.Stack.Cache.Dir, bin.SHA256)); err != nil {
		t.Fatal(err)
	}
	if rec := do(s, "GET", path, pr2Token, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("нет в кэше: ждали 404, получили %d", rec.Code)
	}

	// Своя сборка mtg после итога telemt - с force можно, в ресурсе только mtg.
	fs.machines["pr2"].Telemt.Update.Status = "applied"
	rec = do(s, "POST", "/api/v1/machines/pr2/update", botToken, `{"product":"mtg","version":"1.15.0","force":true}`)
	if rec.Code != http.StatusAccepted || len(fs.content["pr2"]) != 1 || fs.content["pr2"]["mtg"].Version != "1.15.0" {
		t.Fatalf("mtg с force: %d %s %+v", rec.Code, rec.Body.String(), fs.content["pr2"])
	}
}

// Версия «не новее» с force ставится (откат на прошлую версию).
func TestStack_UpdateForceOlder(t *testing.T) {
	s, fs, _ := stackServer(t, false)
	rec := do(s, "POST", "/api/v1/machines/ps2/update", botToken, `{"product":"telemt","version":"3.5.9","force":true}`)
	if rec.Code != http.StatusAccepted || fs.content["ps2"]["telemt"].Version != "3.5.9" {
		t.Fatalf("откат с force: %d %s", rec.Code, rec.Body.String())
	}
}

// Цикл: на мастере проверяет релизы и записывает итог обновлений, на реплике - ничего.
func TestStack_LoopOnlyOnMaster(t *testing.T) {
	s, fs, fr := stackServer(t, false)
	s.stackOnce(context.Background(), true)
	if fr.latestN != 2 || fs.finished != 1 || fs.releases["mtg"].Version == nil {
		t.Fatalf("мастер: запросов %d, проходов %d", fr.latestN, fs.finished)
	}
	replica, rfs, rfr := stackServer(t, true)
	replica.stackOnce(context.Background(), true)
	if rfr.latestN != 0 || rfs.finished != 0 {
		t.Fatal("реплика не должна проверять релизы и писать итог")
	}
}

// Бинаря нет в кэше реплики - 409 not_master: мост и агент идут к следующему адресу.
func TestStack_BlobOnReplica(t *testing.T) {
	s, fs, _ := stackServer(t, true)
	sha := strings.Repeat("a", 64)
	fs.content["pr2"] = stack.Content{"telemt": {Version: "3.5.17", SHA256: sha, Size: 10}}
	rec := do(s, "GET", "/v1/stack/blobs/"+sha+"?target=pr2", pr2Token, "")
	if rec.Code != http.StatusConflict || errCode(t, rec.Body.Bytes()) != "not_master" {
		t.Fatalf("реплика без файла: %d %s", rec.Code, rec.Body.String())
	}
	// Не опубликован этой машине - 404 и на реплике.
	if rec := do(s, "GET", "/v1/stack/blobs/"+strings.Repeat("b", 64)+"?target=pr2", pr2Token, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("чужой sha на реплике: %d", rec.Code)
	}
}

// Пять параллельных обновлений одного релиза на разных машинах - одно скачивание;
// следующее обновление того же релиза берёт бинарь из кэша без загрузки.
func TestStack_ParallelUpdatesDownloadOnce(t *testing.T) {
	s, fs, fr := stackServer(t, false)
	fr.binDelay = 100 * time.Millisecond
	codes := []string{"m1", "m2", "m3", "m4", "m5", "m6"}
	for _, c := range codes {
		fs.machines[c] = store.StackMachine{Code: c, Kind: "exit", Enabled: true,
			Telemt: &store.StackProductInfo{Current: strp("3.5.10")}}
	}
	var wg sync.WaitGroup
	codesCh := make(chan int, 5)
	for _, c := range codes[:5] {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			codesCh <- do(s, "POST", "/api/v1/machines/"+c+"/update", botToken, `{"product":"telemt","version":"3.5.17"}`).Code
		}(c)
	}
	wg.Wait()
	close(codesCh)
	for code := range codesCh {
		if code != http.StatusAccepted {
			t.Fatalf("параллельное обновление: %d", code)
		}
	}
	if fr.binN != 1 {
		t.Fatalf("ждали одно скачивание, было %d", fr.binN)
	}
	if rec := do(s, "POST", "/api/v1/machines/m6/update", botToken, `{"product":"telemt","version":"3.5.17"}`); rec.Code != http.StatusAccepted || fr.binN != 1 {
		t.Fatalf("повтор того же релиза: %d, скачиваний %d", rec.Code, fr.binN)
	}
	// Файл в кэше испорчен - качается заново.
	sha := fs.content["m6"]["telemt"].SHA256
	if err := os.WriteFile(filepath.Join(s.deps.Stack.Cache.Dir, sha), []byte("испорчен"), 0o640); err != nil {
		t.Fatal(err)
	}
	fs.machines["m6"].Telemt.Update.Status = "applied"
	if rec := do(s, "POST", "/api/v1/machines/m6/update", botToken, `{"product":"telemt","version":"3.5.17","force":true}`); rec.Code != http.StatusAccepted || fr.binN != 2 {
		t.Fatalf("испорченный кэш: %d, скачиваний %d", rec.Code, fr.binN)
	}
	if !s.deps.Stack.Cache.Verify(sha) {
		t.Fatal("кэш не восстановлен")
	}
}
