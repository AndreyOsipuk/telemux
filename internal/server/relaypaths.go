package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/AndreyOsipuk/telemux/internal/agentjoin"
	"github.com/AndreyOsipuk/telemux/internal/bridge"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

type relayMasterPath struct {
	Exit   string `json:"exit"`
	Master string `json:"master"`
}

// writeRelayMasters добавляет настройки от лидера. Ошибка получения маршрутов
// не мешает агенту получить бандл; он продолжит использовать принятый список.
func (s *Server) writeRelayMasters(w http.ResponseWriter, r *http.Request, target string) {
	if r.Header.Get(bridge.AcceptRelayMastersHeader) != "1" {
		return
	}
	role, err := s.currentRole(r.Context())
	if err != nil || !role.IsMaster() {
		return
	}
	managed, exits, err := s.deps.Agents.RelayMasterTargets(r.Context(), target)
	if err != nil {
		s.deps.Log.Error("не удалось прочитать пути релея", "target", target, "err", err)
		return
	}
	if !managed {
		return
	}
	seen := make(map[string]struct{}, len(exits))
	list := make([]relayMasterPath, 0, len(exits))
	for _, exit := range exits {
		route, err := agentjoin.BridgeRoute(exit.Settings[store.SettingAddress], exit.Settings[store.SettingBridgeSocks])
		_, duplicate := seen[route.Proxy]
		if err != nil || duplicate || !agentjoin.ValidCode(exit.Code) {
			s.deps.Log.Error("невалидный или повторный путь релея", "target", target, "exit", exit.Code, "err", err)
			return
		}
		seen[route.Proxy] = struct{}{}
		list = append(list, relayMasterPath{Exit: exit.Code, Master: route.Base + "|" + route.Proxy})
	}
	body, err := json.Marshal(struct {
		Paths []relayMasterPath `json:"paths"`
	}{Paths: list})
	if err != nil {
		s.deps.Log.Error("не удалось кодировать пути релея", "target", target, "err", err)
		return
	}
	w.Header().Set(bridge.RelayMastersHeader, base64.RawURLEncoding.EncodeToString(body))
}
