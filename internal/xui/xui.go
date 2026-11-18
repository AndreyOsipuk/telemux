// Package xui - сверка клиентов MTProto-входа мастер-панели 3x-ui (режим mtg
// «через панель»). Мастер-панель сама надёжно доставляет вход до своей ноды
// (флаг config_dirty и сверка раз в 5 с), поэтому telemux приводит к желаемому
// только клиентов входа на мастер-панели. Логика повторяет бота (xui-mtg-sync.ts).
package xui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	// SubIDPrefix - префикс subId клиентов, которыми владеет бот прокси.
	SubIDPrefix = "mtpx-"
	// InboundMark - метка в названии входа: вход принадлежит боту прокси.
	InboundMark = "[mtproxy-bot]"
	// MassRemoveMin - порог защиты от массового удаления.
	MassRemoveMin = 10
	bulkChunk     = 200
)

// Client - клиент MTProto-входа (поля model.Client, нужные для сверки).
type Client struct {
	Email      string `json:"email"`
	Secret     string `json:"secret"`
	SubID      string `json:"subId"`
	Enable     bool   `json:"enable"`
	ExpiryTime int64  `json:"expiryTime"` // мс, 0 - бессрочно
	Comment    string `json:"comment"`
}

// Inbound - MTProto-вход мастер-панели.
type Inbound struct {
	ID       int      `json:"id"`
	Protocol string   `json:"protocol"`
	Port     int      `json:"port"`
	Remark   string   `json:"remark"`
	NodeID   *int     `json:"nodeId"`
	Clients  []Client `json:"-"`
}

// Target - вход, который ведёт telemux.
type Target struct {
	Code      string // код mtg-ноды (суффикс email клиентов)
	InboundID int
	NodeID    int // нода панели, к которой привязан вход
	Port      int // порт входа
}

func emailSuffix(code string) string { return "_" + code }

// IsOwn - клиентом владеет бот прокси (subId mtpx- и суффикс этой ноды).
func IsOwn(c Client, code string) bool {
	return strings.HasPrefix(c.SubID, SubIDPrefix) && strings.HasSuffix(c.Email, emailSuffix(code))
}

// Plan - что изменить во входе.
type Plan struct {
	Add    []Client
	Update []Client
	Remove []string // свои лишние - удаляются целиком
	Detach []string // чужие лишние - снимаются только с этого входа
}

// Empty - менять нечего.
func (p Plan) Empty() bool {
	return len(p.Add)+len(p.Update)+len(p.Remove)+len(p.Detach) == 0
}

// IsMassRemoval - убрать не меньше порога и больше половины входа: скорее сбой
// источника, чем жизнь.
func (p Plan) IsMassRemoval(current int) bool {
	n := len(p.Remove) + len(p.Detach)
	return n >= MassRemoveMin && n*2 > current
}

func differs(cur, want Client) bool {
	return !strings.EqualFold(cur.Secret, want.Secret) || cur.SubID != want.SubID ||
		cur.ExpiryTime != want.ExpiryTime || cur.Enable != want.Enable || cur.Comment != want.Comment
}

// PlanSync - план приведения входа к желаемому набору.
func PlanSync(current, desired []Client, code string) Plan {
	cur := make(map[string]Client, len(current))
	for _, c := range current {
		cur[c.Email] = c
	}
	want := make(map[string]bool, len(desired))
	var p Plan
	for _, w := range desired {
		want[w.Email] = true
		if c, ok := cur[w.Email]; !ok {
			p.Add = append(p.Add, w)
		} else if differs(c, w) {
			p.Update = append(p.Update, w)
		}
	}
	for _, c := range current {
		if want[c.Email] {
			continue
		}
		if IsOwn(c, code) {
			p.Remove = append(p.Remove, c.Email)
		} else {
			p.Detach = append(p.Detach, c.Email)
		}
	}
	return p
}

// Digest - отпечаток включённых клиентов, как его отдаёт mtg-сайдкар панели в
// /stats (secrets_sha256): sha256 строк "email=секрет" по email через \n.
func Digest(clients []Client) string {
	var rows []string
	for _, c := range clients {
		if c.Enable {
			rows = append(rows, c.Email+"="+strings.ToLower(c.Secret))
		}
	}
	sort.Strings(rows)
	sum := sha256.Sum256([]byte(strings.Join(rows, "\n")))
	return hex.EncodeToString(sum[:])
}

// API - клиент REST мастер-панели. Токен - только в заголовке, в ошибки не попадает.
type API struct {
	base   string
	token  string
	client *http.Client
}

// NewAPI создаёт клиент. tlsConf - закрепление сертификата панели (nil - обычная проверка).
func NewAPI(base, token string, tlsConf *tls.Config) *API {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if tlsConf != nil {
		tr.TLSClientConfig = tlsConf
	}
	return &API{base: strings.TrimRight(base, "/"), token: token, client: &http.Client{Timeout: 30 * time.Second, Transport: tr}}
}

// PinnedTLS - проверка сертификата панели по sha256 отпечатку (самоподписанный по IP).
func PinnedTLS(sha256Hex string) *tls.Config {
	want := strings.ToLower(strings.ReplaceAll(sha256Hex, ":", ""))
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // проверка - по отпечатку ниже
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("панель не предъявила сертификат")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if hex.EncodeToString(sum[:]) != want {
				return errors.New("отпечаток сертификата панели не совпал")
			}
			return nil
		},
	}
}

type envelope struct {
	Success bool            `json:"success"`
	Msg     string          `json:"msg"`
	Obj     json.RawMessage `json:"obj"`
}

func (a *API) call(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+"/panel/api"+path, rd)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: панель ответила %d", method, path, resp.StatusCode)
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("%s %s: ответ не JSON", method, path)
	}
	if !env.Success {
		return nil, fmt.Errorf("%s %s: %s", method, path, env.Msg)
	}
	return env.Obj, nil
}

// GetInbound - вход с клиентами.
func (a *API) GetInbound(ctx context.Context, id int) (Inbound, error) {
	raw, err := a.call(ctx, http.MethodGet, fmt.Sprintf("/inbounds/get/%d", id), nil)
	if err != nil {
		return Inbound{}, err
	}
	var in struct {
		Inbound
		Settings json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Inbound{}, fmt.Errorf("вход %d не читается: %w", id, err)
	}
	// settings - объект (3.3+) или JSON-строка (старые панели).
	settings := in.Settings
	var asString string
	if json.Unmarshal(settings, &asString) == nil {
		settings = json.RawMessage(asString)
	}
	var s struct {
		Clients []Client `json:"clients"`
	}
	if len(settings) > 0 {
		if err := json.Unmarshal(settings, &s); err != nil {
			return Inbound{}, fmt.Errorf("клиенты входа %d не читаются: %w", id, err)
		}
	}
	in.Inbound.Clients = s.Clients
	return in.Inbound, nil
}

// Result - итог сверки входа.
type Result struct {
	Changed bool
	Digest  string // отпечаток желаемого набора (для проверки на ноде)
	Error   string
}

func checkInbound(in Inbound, t Target) error {
	switch {
	case in.Protocol != "mtproto":
		return fmt.Errorf("вход %d - %q, а не mtproto", in.ID, in.Protocol)
	case in.Port != t.Port:
		return fmt.Errorf("вход %d слушает порт %d, а нода - %d", in.ID, in.Port, t.Port)
	case in.NodeID == nil || *in.NodeID != t.NodeID:
		return fmt.Errorf("вход %d привязан не к ноде %d", in.ID, t.NodeID)
	case !strings.Contains(in.Remark, InboundMark):
		return fmt.Errorf("в названии входа %d нет метки %s", in.ID, InboundMark)
	}
	return nil
}

// Sync приводит клиентов входа к желаемому набору. Без расхождений не делает ни
// одного запроса на запись; после записи перечитывает вход и сверяет.
func Sync(ctx context.Context, api *API, t Target, desired []Client, allowMassRemove bool) Result {
	res := Result{Digest: Digest(desired)}
	in, err := api.GetInbound(ctx, t.InboundID)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if err := checkInbound(in, t); err != nil {
		res.Error = err.Error()
		return res
	}
	plan := PlanSync(in.Clients, desired, t.Code)
	if plan.Empty() {
		return res
	}
	if plan.IsMassRemoval(len(in.Clients)) && !allowMassRemove {
		res.Error = fmt.Sprintf("сверка убрала бы %d из %d клиентов входа - похоже на сбой источника, изменения не применены",
			len(plan.Remove)+len(plan.Detach), len(in.Clients))
		return res
	}
	res.Changed = true
	var errs []string
	// Операции независимы: сбой одной не мешает остальным.
	for _, email := range plan.Detach {
		if _, err := api.call(ctx, http.MethodPost, "/clients/"+url.PathEscape(email)+"/detach", map[string]any{"inboundIds": []int{t.InboundID}}); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(plan.Remove) > 0 {
		if _, err := api.call(ctx, http.MethodPost, "/clients/bulkDel", map[string]any{"emails": plan.Remove, "keepTraffic": false}); err != nil {
			errs = append(errs, err.Error())
		}
	}
	for _, c := range plan.Update {
		if _, err := api.call(ctx, http.MethodPost, "/clients/update/"+url.PathEscape(c.Email), c); err != nil {
			errs = append(errs, err.Error())
		}
	}
	for i := 0; i < len(plan.Add); i += bulkChunk {
		chunk := plan.Add[i:min(i+bulkChunk, len(plan.Add))]
		items := make([]map[string]any, len(chunk))
		for j, c := range chunk {
			items[j] = map[string]any{"client": c, "inboundIds": []int{t.InboundID}}
		}
		if _, err := api.call(ctx, http.MethodPost, "/clients/bulkCreate", items); err != nil {
			errs = append(errs, err.Error())
		}
	}
	// Перечитать и сверить: панель могла молча пропустить клиента.
	after, err := api.GetInbound(ctx, t.InboundID)
	if err != nil {
		errs = append(errs, "перечитать вход: "+err.Error())
	} else if rest := PlanSync(after.Clients, desired, t.Code); !rest.Empty() {
		errs = append(errs, fmt.Sprintf("после сверки во входе осталось расхождение: добавить %d, обновить %d, убрать %d",
			len(rest.Add), len(rest.Update), len(rest.Remove)+len(rest.Detach)))
	}
	if len(errs) > 0 {
		res.Error = strings.Join(errs, "; ")
	}
	return res
}
