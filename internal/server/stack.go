package server

// Обновление telemt и mtg-multi кнопкой (ADR 0008): последние релизы GitHub, версии на
// машинах, обновление продукта на машине (ресурс stack) и бинарь для агента. Чтение -
// токен бота или вход в панель; проверка релизов и обновление - то же и только на мастере;
// бинарь - токен агента машины.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/AndreyOsipuk/telemux/internal/stack"
	"github.com/AndreyOsipuk/telemux/internal/stackrel"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

// StackStore - релизы, версии машин и обновления (реализует store.Store).
type StackStore interface {
	StackReleases(ctx context.Context) ([]store.StackRelease, error)
	SaveStackRelease(ctx context.Context, c store.StackReleaseCheck) error
	StackMachines(ctx context.Context, limit, offset int) ([]store.StackMachine, int, error)
	StackMachine(ctx context.Context, code string) (store.StackMachine, error)
	PublishStackUpdate(ctx context.Context, code, product string, bin stack.Binary, from *string) (store.StackUpdate, error)
	StackBlobAllowed(ctx context.Context, target, sha string) (bool, error)
	FinishStackUpdates(ctx context.Context) ([]store.StackUpdate, error)
}

// StackReleases - релизы продуктов (реализует *stackrel.Client; тесты подменяют).
type StackReleases interface {
	Repo(product string) string
	Latest(ctx context.Context, product string) (stackrel.Release, error)
	ByVersion(ctx context.Context, product, version string) (stackrel.Release, error)
	Binary(ctx context.Context, rel stackrel.Release, arch string) ([]byte, error)
}

// StackDeps - зависимости обновления продуктов.
type StackDeps struct {
	Store    StackStore
	Releases StackReleases
	Cache    stackrel.Cache // бинари на диске мастера
}

const (
	// stackCheckEvery - как часто мастер проверяет релизы (и при старте).
	stackCheckEvery = 6 * time.Hour
	// stackFinishEvery - как часто мастер записывает итог обновлений по отчётам агентов.
	stackFinishEvery = 30 * time.Second
	// stackCheckMinGap - проверка релизов по кнопке не чаще.
	stackCheckMinGap = time.Minute
	// maxStackUpdateBody - тело POST /api/v1/machines/{code}/update.
	maxStackUpdateBody = 4 << 10
)

// stackState - ограничение частоты проверки по кнопке, сериализация проверок и
// скачивание релизов: одно одновременно (память и лимиты GitHub), одинаковые запросы
// склеиваются, уже скачанный бинарь берётся из кэша на диске.
type stackState struct {
	mu         sync.Mutex
	lastManual time.Time
	checkMu    sync.Mutex

	fetchSem  chan struct{}           // одна загрузка за раз (создаётся лениво под mu)
	fetchSF   singleflight.Group      // ключ - (repo, tag, архив)
	fetchMemo map[string]stackBlobRef // (repo, tag, архив) -> бинарь в кэше
}

// stackBlobRef - бинарь релиза в кэше.
type stackBlobRef struct {
	sha  string
	size int64
}

// stackCacheError - сбой записи в кэш на диске мастера (500, а не 503 release_unavailable).
type stackCacheError struct{ err error }

func (e *stackCacheError) Error() string { return "кэш бинарей: " + e.err.Error() }
func (e *stackCacheError) Unwrap() error { return e.err }

// stackStoreError - сбой базы при выборе релиза (writeStoreErr, а не release_unavailable).
type stackStoreError struct{ err error }

func (e *stackStoreError) Error() string { return e.err.Error() }
func (e *stackStoreError) Unwrap() error { return e.err }

func (st *stackState) memo(key string) (stackBlobRef, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	ref, ok := st.fetchMemo[key]
	return ref, ok
}

func (st *stackState) remember(key string, ref stackBlobRef) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.fetchMemo == nil {
		st.fetchMemo = map[string]stackBlobRef{}
	}
	st.fetchMemo[key] = ref
}

func (st *stackState) sem() chan struct{} {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.fetchSem == nil {
		st.fetchSem = make(chan struct{}, 1)
	}
	return st.fetchSem
}

// stackFetchTimeout - предел одной общей загрузки релиза (ожидание семафора и скачивание).
const stackFetchTimeout = 6 * time.Minute

// fetchBinary - бинарь релиза для архитектуры в кэше мастера: SHA256 и размер. Если этот
// архив уже скачивался и бинарь с тем же хэшем лежит в кэше - без загрузки. Иначе
// скачивание под семафором на одну загрузку; параллельные запросы того же архива ждут
// одну загрузку (singleflight).
func (s *Server) fetchBinary(ctx context.Context, rel stackrel.Release, arch string) (string, int64, error) {
	files, err := stackrel.FilesFor(rel, arch)
	if err != nil {
		return "", 0, err
	}
	key := rel.Repo + "|" + rel.Tag + "|" + files.Archive
	cache := s.deps.Stack.Cache
	st := &s.stackSt
	if ref, ok := st.memo(key); ok && cache.Verify(ref.sha) {
		return ref.sha, ref.size, nil
	}
	v, err, _ := st.fetchSF.Do(key, func() (any, error) {
		if ref, ok := st.memo(key); ok && cache.Verify(ref.sha) {
			return ref, nil
		}
		// Загрузку ждут все параллельные запросы этого архива: отмена первого запроса
		// (клиент закрыл вкладку) не должна обрывать её остальным. Свой предел - 6 минут.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stackFetchTimeout)
		defer cancel()
		sem := st.sem()
		select {
		case sem <- struct{}{}:
		case <-dctx.Done():
			return nil, dctx.Err()
		}
		defer func() { <-sem }()
		data, err := s.deps.Stack.Releases.Binary(dctx, rel, arch)
		if err != nil {
			return nil, err
		}
		sha, size, err := cache.Put(data)
		if err != nil {
			return nil, &stackCacheError{err: err}
		}
		ref := stackBlobRef{sha: sha, size: size}
		st.remember(key, ref)
		return ref, nil
	})
	if err != nil {
		return "", 0, err
	}
	ref := v.(stackBlobRef)
	return ref.sha, ref.size, nil
}

// allowManual - можно ли проверить релизы по кнопке сейчас; да - запоминает время.
func (st *stackState) allowManual(now time.Time) (bool, time.Duration) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.lastManual.IsZero() {
		if wait := stackCheckMinGap - now.Sub(st.lastManual); wait > 0 {
			return false, wait
		}
	}
	st.lastManual = now
	return true, 0
}

// stackLatest - последний релиз в ответе.
type stackLatest struct {
	Version     string    `json:"version"`
	Tag         string    `json:"tag"`
	PublishedAt time.Time `json:"published_at"`
}

// stackProduct - схема StackProduct.
type stackProduct struct {
	Product   string       `json:"product"`
	Repo      string       `json:"repo"`
	Latest    *stackLatest `json:"latest"`
	CheckedAt *time.Time   `json:"checked_at"`
	Error     *string      `json:"error"`
}

// stackProductState - схема StackProductState.
type stackProductState struct {
	Current         *string            `json:"current"`
	Latest          *string            `json:"latest"`
	UpdateAvailable bool               `json:"update_available"`
	CustomBuild     bool               `json:"custom_build"`
	Update          *store.StackUpdate `json:"update"`
}

// stackMachine - схема StackMachine.
type stackMachine struct {
	Code    string             `json:"code"`
	Kind    string             `json:"kind"`
	Enabled bool               `json:"enabled"`
	Telemt  *stackProductState `json:"telemt"`
	Mtg     *stackProductState `json:"mtg"`
}

// stackUpdateBody - схема StackUpdateRequest.
type stackUpdateBody struct {
	Product string `json:"product"`
	Version string `json:"version"`
	Force   bool   `json:"force"`
}

// stackProducts - последние релизы всех продуктов: продукта нет в базе - latest null.
func (s *Server) stackProducts(ctx context.Context) ([]stackProduct, error) {
	rels, err := s.deps.Stack.Store.StackReleases(ctx)
	if err != nil {
		return nil, err
	}
	byProduct := map[string]store.StackRelease{}
	for _, r := range rels {
		byProduct[r.Product] = r
	}
	out := make([]stackProduct, 0, len(stack.Products))
	for _, p := range stack.Products {
		sp := stackProduct{Product: p, Repo: s.deps.Stack.Releases.Repo(p)}
		if r, ok := byProduct[p]; ok {
			sp.Repo, sp.CheckedAt, sp.Error = r.Repo, utcTime(r.CheckedAt), r.Error
			if r.Version != nil {
				l := &stackLatest{Version: *r.Version}
				if r.Tag != nil {
					l.Tag = *r.Tag
				}
				if r.PublishedAt != nil {
					l.PublishedAt = r.PublishedAt.UTC()
				}
				sp.Latest = l
			}
		}
		out = append(out, sp)
	}
	return out, nil
}

func utcTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

// latestVersions - продукт -> версия последнего релиза (только узнанные).
func latestVersions(products []stackProduct) map[string]string {
	out := map[string]string{}
	for _, p := range products {
		if p.Latest != nil {
			out[p.Product] = p.Latest.Version
		}
	}
	return out
}

// productState - состояние продукта машины против последнего релиза; nil - продукта нет.
func productState(info *store.StackProductInfo, latest string, known bool) *stackProductState {
	if info == nil {
		return nil
	}
	st := &stackProductState{Current: info.Current, Update: info.Update}
	if known {
		st.Latest = &latest
	}
	if info.Current != nil {
		a := stack.Assess(*info.Current, latest)
		st.CustomBuild = a.CustomBuild
		st.UpdateAvailable = known && a.UpdateAvailable
	}
	return st
}

// checkStackReleases узнаёт последний релиз каждого продукта и сохраняет итог. Ошибка
// GitHub по продукту - в его error (прошлый релиз остаётся), ошибка базы - наружу.
func (s *Server) checkStackReleases(ctx context.Context) error {
	s.stackSt.checkMu.Lock()
	defer s.stackSt.checkMu.Unlock()
	for _, p := range stack.Products {
		c := store.StackReleaseCheck{Product: p, Repo: s.deps.Stack.Releases.Repo(p)}
		rel, err := s.deps.Stack.Releases.Latest(ctx, p)
		if err != nil {
			s.deps.Log.Warn("релизы: последний релиз не узнан, остаётся прошлый", "product", p, "repo", c.Repo, "err", err)
			c.Err = err.Error()
		} else {
			c.Repo, c.Version, c.Tag, c.PublishedAt = rel.Repo, rel.Version, rel.Tag, rel.PublishedAt
		}
		if err := s.deps.Stack.Store.SaveStackRelease(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// stackOnce - проход цикла на мастере: проверка релизов (если check) и итог обновлений.
func (s *Server) stackOnce(ctx context.Context, check bool) {
	st, err := s.currentRole(ctx)
	if err != nil || !st.IsMaster() {
		return
	}
	if check {
		if err := s.checkStackReleases(ctx); err != nil {
			s.deps.Log.Error("релизы: итог проверки не сохранён, повтор при следующем проходе", "err", err)
		}
	}
	done, err := s.deps.Stack.Store.FinishStackUpdates(ctx)
	if err != nil {
		s.deps.Log.Error("обновления: итог не записан, повтор через 30 с", "err", err)
		return
	}
	for _, u := range done {
		if u.Status == "failed" {
			msg := ""
			if u.Error != nil {
				msg = *u.Error
			}
			s.deps.Log.Warn("обновление не применено", "target", u.Code, "product", u.Product, "to", u.To,
				"revision", u.Revision, "error", msg)
			continue
		}
		s.deps.Log.Info("обновление применено", "target", u.Code, "product", u.Product, "to", u.To, "revision", u.Revision)
	}
}

// stackLoop - проверка релизов при старте и раз в stackCheckEvery, итог обновлений раз
// в stackFinishEvery. Работает только на мастере (роль проверяется на каждом проходе).
func (s *Server) stackLoop(ctx context.Context) {
	finish := time.NewTicker(stackFinishEvery)
	defer finish.Stop()
	var lastCheck time.Time
	for {
		now := time.Now()
		check := lastCheck.IsZero() || now.Sub(lastCheck) >= stackCheckEvery
		if check {
			if st, err := s.currentRole(ctx); err == nil && st.IsMaster() {
				lastCheck = now
			} else {
				check = false // реплика: проверит, когда станет мастером
			}
		}
		s.stackOnce(ctx, check)
		select {
		case <-ctx.Done():
			return
		case <-finish.C:
		}
	}
}

// writeReleaseUnavailable - 503 release_unavailable: причина - в журнал, клиенту - общий текст.
func (s *Server) writeReleaseUnavailable(w http.ResponseWriter, code, product string, err error) {
	s.deps.Log.Error("обновление: релиз не получен", "target", code, "product", product, "err", err)
	msg := "релиз не скачался или контрольная сумма не сошлась, повторите позже"
	writeJSONStatus(w, http.StatusServiceUnavailable, errorBody{Code: "release_unavailable", Message: msg, Error: msg})
}

// resolveRelease - релиз для обновления: по версии или последний известный (нет - узнаётся
// сейчас). Ошибка ввода - codedError (400 unknown_version), иначе - релиз недоступен.
func (s *Server) resolveRelease(ctx context.Context, product, version string) (stackrel.Release, error) {
	rels := s.deps.Stack.Releases
	if version != "" {
		rel, err := rels.ByVersion(ctx, product, version)
		if errors.Is(err, stackrel.ErrReleaseNotFound) {
			return rel, codedErr("unknown_version", fmt.Errorf("у %s нет релиза %q, который можно поставить", product, version))
		}
		return rel, err
	}
	known, err := s.deps.Stack.Store.StackReleases(ctx)
	if err != nil {
		return stackrel.Release{}, &stackStoreError{err: err}
	}
	for _, r := range known {
		if r.Product == product && r.Tag != nil {
			return rels.ByVersion(ctx, product, *r.Tag)
		}
	}
	rel, err := rels.Latest(ctx, product)
	if err != nil {
		return rel, err
	}
	if err := s.deps.Stack.Store.SaveStackRelease(ctx, store.StackReleaseCheck{Product: product, Repo: rel.Repo,
		Version: rel.Version, Tag: rel.Tag, PublishedAt: rel.PublishedAt}); err != nil {
		s.deps.Log.Warn("релизы: последний релиз не сохранён", "product", product, "err", err)
	}
	return rel, nil
}

func (s *Server) routesStack() {
	// Последние релизы и версии на машинах страницей.
	s.mux.HandleFunc("GET /api/v1/stack", func(w http.ResponseWriter, r *http.Request) {
		limit, offset, ok := s.readPage(w, r)
		if !ok {
			return
		}
		products, err := s.stackProducts(r.Context())
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		st, err := s.currentRole(r.Context())
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err, s.deps.Log)
			return
		}
		data := []stackMachine{}
		total := 0
		if st.IsMaster() {
			list, n, err := s.deps.Stack.Store.StackMachines(r.Context(), limit, offset)
			if err != nil {
				s.writeStoreErr(w, err)
				return
			}
			total = n
			latest := latestVersions(products)
			for _, m := range list {
				lt, okT := latest[stack.ProductTelemt]
				lm, okM := latest[stack.ProductMtg]
				data = append(data, stackMachine{Code: m.Code, Kind: m.Kind, Enabled: m.Enabled,
					Telemt: productState(m.Telemt, lt, okT), Mtg: productState(m.Mtg, lm, okM)})
			}
		}
		writeJSON(w, map[string]any{
			"products": products,
			"data":     data,
			"paging":   map[string]int{"total": total, "limit": limit, "offset": offset},
		})
	})

	// Проверить релизы сейчас (не чаще раза в минуту).
	s.mux.HandleFunc("POST /api/v1/stack/check", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) || !s.requireMaster(w, r) {
			return
		}
		if ok, wait := s.stackSt.allowManual(time.Now()); !ok {
			sec := int(wait/time.Second) + 1
			w.Header().Set("Retry-After", strconv.Itoa(sec))
			writeErr(w, http.StatusTooManyRequests, codedErr("too_many_requests",
				fmt.Errorf("релизы проверяются не чаще раза в минуту, повторите через %d с", sec)), s.deps.Log)
			return
		}
		if err := s.checkStackReleases(r.Context()); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		products, err := s.stackProducts(r.Context())
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"products": products})
	})

	// Обновить продукт на машине: скачать релиз, проверить, опубликовать ресурс stack.
	s.mux.HandleFunc("POST /api/v1/machines/{code}/update", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireBotOrSession(w, r) {
			return
		}
		code, ok := machineCode(r)
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет машины %q", code), s.deps.Log)
			return
		}
		var body stackUpdateBody
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxStackUpdateBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeErr(w, http.StatusRequestEntityTooLarge, fmt.Errorf("тело больше %d байт", maxStackUpdateBody), s.deps.Log)
				return
			}
			writeErr(w, http.StatusBadRequest, fmt.Errorf("нужен JSON {product, version?, force?}: %w", err), s.deps.Log)
			return
		}
		if !stack.ValidProduct(body.Product) {
			writeErr(w, http.StatusBadRequest, codedErr("unknown_product",
				fmt.Errorf("product %q: допустимы telemt, mtg", body.Product)), s.deps.Log)
			return
		}
		if body.Version != "" && !stackrel.ValidVersion(body.Version) {
			writeErr(w, http.StatusBadRequest, codedErr("unknown_version",
				fmt.Errorf("version %q: буквы, цифры, . _ + -, до 64 символов", body.Version)), s.deps.Log)
			return
		}
		if !s.requireMaster(w, r) {
			return
		}
		ctx := r.Context()
		m, err := s.deps.Stack.Store.StackMachine(ctx, code)
		if errors.Is(err, store.ErrUnknownTarget) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет машины %q", code), s.deps.Log)
			return
		}
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		if !m.Enabled {
			writeErr(w, http.StatusConflict, codedErr("machine_disabled",
				fmt.Errorf("машина %q выключена: обновления ей не публикуются", code)), s.deps.Log)
			return
		}
		info := m.Telemt
		if body.Product == stack.ProductMtg {
			info = m.Mtg
		}
		if info == nil {
			writeErr(w, http.StatusConflict, codedErr("product_missing",
				fmt.Errorf("агент машины %q не присылает %s: продукта на машине нет или агент не отчитывался", code, body.Product)), s.deps.Log)
			return
		}
		// Одно обновление на машине за раз (по любому продукту): итог ревизии stack
		// относится к одному продукту.
		for _, p := range []*store.StackProductInfo{m.Telemt, m.Mtg} {
			if p != nil && p.Update != nil && p.Update.Status == "pending" {
				writeErr(w, http.StatusConflict, codedErr("update_in_progress",
					fmt.Errorf("на %q ещё идёт обновление %s до %s (ревизия %d)", code, p.Update.Product, p.Update.To, p.Update.Revision)), s.deps.Log)
				return
			}
		}
		arch, err := stackrel.Arch(m.Arch)
		if err != nil {
			writeErr(w, http.StatusConflict, codedErr("asset_missing", fmt.Errorf("машина %q: %w", code, err)), s.deps.Log)
			return
		}
		rel, err := s.resolveRelease(ctx, body.Product, body.Version)
		var ce *codedError
		var se *stackStoreError
		switch {
		case errors.As(err, &ce):
			writeErr(w, http.StatusBadRequest, err, s.deps.Log)
			return
		case errors.As(err, &se):
			s.writeStoreErr(w, se.err)
			return
		}
		if err != nil {
			s.writeReleaseUnavailable(w, code, body.Product, err)
			return
		}
		if info.Current != nil && !body.Force {
			a := stack.Assess(*info.Current, rel.Version)
			if a.CustomBuild {
				writeErr(w, http.StatusConflict, codedErr("custom_build",
					fmt.Errorf("на %q своя сборка %s %s: обновить на %s можно только с force", code, body.Product, *info.Current, rel.Version)), s.deps.Log)
				return
			}
			if !a.UpdateAvailable {
				writeErr(w, http.StatusConflict, codedErr("not_newer",
					fmt.Errorf("%s %s не новее версии на %q (%s): поставить можно только с force", body.Product, rel.Version, code, *info.Current)), s.deps.Log)
				return
			}
		}
		sha, size, err := s.fetchBinary(ctx, rel, arch)
		var cacheErr *stackCacheError
		switch {
		case errors.Is(err, stackrel.ErrAssetMissing), errors.Is(err, stackrel.ErrUnsupportedArch):
			writeErr(w, http.StatusConflict, codedErr("asset_missing",
				fmt.Errorf("в релизе %s %s нет файла для машины %q (%s): %w", body.Product, rel.Tag, code, arch, err)), s.deps.Log)
			return
		case errors.As(err, &cacheErr):
			writeErr(w, http.StatusInternalServerError, err, s.deps.Log)
			return
		case err != nil:
			s.writeReleaseUnavailable(w, code, body.Product, err)
			return
		}
		u, err := s.deps.Stack.Store.PublishStackUpdate(ctx, code, body.Product,
			stack.Binary{Version: rel.Version, SHA256: sha, Size: size}, info.Current)
		switch {
		case errors.Is(err, store.ErrUnknownTarget):
			writeErr(w, http.StatusNotFound, fmt.Errorf("нет машины %q", code), s.deps.Log)
			return
		case err != nil:
			s.writeStoreErr(w, err)
			return
		}
		s.deps.Log.Info("обновление опубликовано", "target", code, "product", body.Product, "from", ptrStr(info.Current),
			"to", rel.Version, "sha256", sha, "size", size, "revision", u.Revision, "force", body.Force)
		s.revs.bump()
		writeJSONStatus(w, http.StatusAccepted, u)
	})

	// Бинарь обновления для агента: только опубликованный этой машине в ресурсе stack.
	s.mux.HandleFunc("GET /v1/stack/blobs/{sha256}", func(w http.ResponseWriter, r *http.Request) {
		code := s.agentTarget(w, r)
		if code == "" {
			return
		}
		if r.URL.Query().Get("target") != code {
			writeErr(w, http.StatusForbidden, fmt.Errorf("токен выдан машине %q", code), s.deps.Log)
			return
		}
		sha := r.PathValue("sha256")
		if !stack.ValidSHA256(sha) {
			writeErr(w, http.StatusBadRequest, errors.New("sha256 - 64 строчных hex-символа"), s.deps.Log)
			return
		}
		allowed, err := s.deps.Stack.Store.StackBlobAllowed(r.Context(), code, sha)
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		if !allowed {
			writeErr(w, http.StatusNotFound, fmt.Errorf("бинарь %s машине %q не публиковался", sha, code), s.deps.Log)
			return
		}
		f, size, err := s.deps.Stack.Cache.Open(sha)
		if errors.Is(err, fs.ErrNotExist) {
			// Кэш есть только у мастера, который скачал бинарь. Не мастер - 409 not_master:
			// мост и агент идут к следующему адресу.
			st, rerr := s.currentRole(r.Context())
			if rerr != nil {
				writeErr(w, http.StatusServiceUnavailable, rerr, s.deps.Log)
				return
			}
			if !st.IsMaster() {
				s.writeNotMaster(w, st)
				return
			}
			writeErr(w, http.StatusNotFound, fmt.Errorf("бинаря %s нет в кэше мастера: повторите обновление", sha), s.deps.Log)
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("кэш бинарей: %w", err), s.deps.Log)
			return
		}
		defer f.Close() //nolint:errcheck
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		if _, err := io.Copy(w, f); err != nil {
			s.deps.Log.Warn("бинарь агенту отдан не полностью", "target", code, "sha256", sha, "err", err)
		}
	})
}

func ptrStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
