package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

func (f *fakeAgents) Machines(_ context.Context, flt store.MachineFilter, limit, offset int) ([]store.Machine, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastLimit, f.lastOffset, f.lastFilter = limit, offset, flt
	var all []store.Machine
	for _, m := range f.machines {
		if flt.Kind != "" && m.Kind != flt.Kind || flt.Enabled != nil && m.Enabled != *flt.Enabled ||
			flt.Resource != "" && !slices.Contains(m.Resources, flt.Resource) {
			continue
		}
		all = append(all, m)
	}
	if offset >= len(all) {
		return []store.Machine{}, len(all), nil
	}
	end := min(offset+limit, len(all))
	return all[offset:end], len(all), nil
}

// Машина без ресурсов отчитывается только о себе и проверках - мастер принимает.
func TestAgentAPI_ObservedMachineOnly(t *testing.T) {
	s, _, fa := agentServer(false)
	body := `{"target":"pr2","agent_version":"v1","resources":[],"checks":[{"name":"bridge","ok":false,"error":"refused"}]}`
	if rec := do(s, "POST", "/v1/observed", pr2Token, body); rec.Code != http.StatusNoContent {
		t.Fatalf("ждали 204, получили %d: %s", rec.Code, rec.Body.String())
	}
	if len(fa.observed) != 1 || len(fa.observed[0].Checks) != 1 || fa.observed[0].Checks[0].OK {
		t.Fatalf("отчёт о машине не сохранён: %+v", fa.observed)
	}
	if rec := do(s, "POST", "/v1/observed", pr2Token, `{"target":"pr2","resources":[]}`); rec.Code != http.StatusNoContent {
		t.Fatalf("пустой отчёт (агент жив) → ждали 204, получили %d", rec.Code)
	}
	for _, bad := range []string{
		`{"target":"pr2","resources":[],"checks":[{"name":"","ok":true}]}`,
		`{"target":"pr2","resources":[],"checks":[` + repeatChecks(21) + `]}`,
	} {
		if rec := do(s, "POST", "/v1/observed", pr2Token, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("битые проверки → ждали 400, получили %d", rec.Code)
		}
	}
}

func repeatChecks(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ","
		}
		out += `{"name":"c","ok":true}`
	}
	return out
}

func TestV1_Machines(t *testing.T) {
	s, _, fa := agentServer(false)
	now := time.Now().UTC()
	for _, c := range []string{"pr1", "ps2", "vega"} {
		fa.machines = append(fa.machines, store.Machine{Code: c, Kind: "exit", Resources: []string{}, SeenAt: &now,
			Checks: []store.ObservedCheck{}, Observed: []store.MachineResource{}})
	}
	if rec := do(s, "GET", "/api/v1/machines", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("без токена → ждали 401, получили %d", rec.Code)
	}
	rec := do(s, "GET", "/api/v1/machines?limit=2&offset=1", botToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("ждали 200, получили %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data   []store.Machine                    `json:"data"`
		Paging struct{ Total, Limit, Offset int } `json:"paging"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 2 || resp.Data[0].Code != "ps2" || resp.Paging.Total != 3 || resp.Paging.Limit != 2 || resp.Paging.Offset != 1 {
		t.Fatalf("не та страница: %+v", resp)
	}
	if rec := do(s, "GET", "/api/v1/machines", botToken, ""); rec.Code != http.StatusOK || fa.lastLimit != 20 || fa.lastOffset != 0 {
		t.Fatalf("по умолчанию ждали limit 20 offset 0, получили %d %d (%d)", fa.lastLimit, fa.lastOffset, rec.Code)
	}
	for _, q := range []string{"limit=0", "limit=101", "limit=x", "offset=-1", "offset=x",
		"resource=nope", "resource=telemt", "kind=bridge", "enabled=yes", "enabled=1"} {
		if rec := do(s, "GET", "/api/v1/machines?"+q, botToken, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s → ждали 400, получили %d", q, rec.Code)
		}
	}
}

// Фильтры списка машин: resource, enabled, kind; total считается с фильтром.
func TestV1_MachinesFilters(t *testing.T) {
	s, _, fa := agentServer(false)
	add := func(code, kind string, enabled bool, res ...string) {
		if res == nil {
			res = []string{}
		}
		fa.machines = append(fa.machines, store.Machine{Code: code, Kind: kind, Enabled: enabled, Resources: res,
			Checks: []store.ObservedCheck{}, Observed: []store.MachineResource{}})
	}
	add("pr1", "relay", true, store.ResourceTelemtUsers)
	add("pr2", "relay", false, store.ResourceTelemtUsers)
	add("ps1", "exit", true, store.ResourceTelemtUsers, store.ResourceMtgConfig)
	add("ps5", "exit", true, store.ResourceMtgConfig)
	add("vega", "exit", true)
	add("xui7", "panel", true, store.ResourceXuiClients)

	cases := []struct {
		q     string
		total int
		first string
	}{
		{"resource=telemt.users", 3, "pr1"},
		{"resource=telemt.users&enabled=true", 2, "pr1"},
		{"resource=telemt.users&enabled=true&kind=exit", 1, "ps1"},
		{"resource=mtg.config&limit=1&offset=1", 2, "ps5"},
		{"enabled=false", 1, "pr2"},
		{"kind=panel", 1, "xui7"},
	}
	for _, c := range cases {
		rec := do(s, "GET", "/api/v1/machines?"+c.q, botToken, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: ждали 200, получили %d: %s", c.q, rec.Code, rec.Body.String())
		}
		var resp struct {
			Data   []store.Machine                    `json:"data"`
			Paging struct{ Total, Limit, Offset int } `json:"paging"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Paging.Total != c.total || len(resp.Data) == 0 || resp.Data[0].Code != c.first {
			t.Fatalf("%s: ждали total=%d и первую %s, получили %+v", c.q, c.total, c.first, resp)
		}
	}
	if fa.lastFilter.Kind != "panel" || fa.lastFilter.Enabled != nil {
		t.Fatalf("фильтр не дошёл до хранилища: %+v", fa.lastFilter)
	}
}
