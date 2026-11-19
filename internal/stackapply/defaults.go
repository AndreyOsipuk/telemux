package stackapply

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AndreyOsipuk/telemux/internal/stack"
)

// Target - что заменять и как проверять для продукта.
type Target struct {
	Bin       string // имя или путь бинаря
	Unit      string
	HealthURL string
}

// Defaults - бинарь, юнит и адрес проверки продукта из окружения агента
// (те же переменные, что у `telemux agent`):
//   - telemt: TELEMUX_AGENT_TELEMT_BIN (telemt), TELEMUX_AGENT_TELEMT_UNIT (telemt),
//     TELEMUX_AGENT_TELEMT_API (http://127.0.0.1:9091) + /v1/health;
//   - mtg: TELEMUX_AGENT_MTG_BIN (mtg-multi), TELEMUX_AGENT_MTG_UNIT (mtg-multi),
//     TELEMUX_AGENT_MTG_STATS_URL (http://127.0.0.1:8081/stats).
func Defaults(product string, getenv func(string) string) (Target, error) {
	or := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	switch product {
	case stack.ProductTelemt:
		return Target{
			Bin:       or("TELEMUX_AGENT_TELEMT_BIN", "telemt"),
			Unit:      or("TELEMUX_AGENT_TELEMT_UNIT", "telemt"),
			HealthURL: strings.TrimRight(or("TELEMUX_AGENT_TELEMT_API", "http://127.0.0.1:9091"), "/") + "/v1/health",
		}, nil
	case stack.ProductMtg:
		return Target{
			Bin:       or("TELEMUX_AGENT_MTG_BIN", "mtg-multi"),
			Unit:      or("TELEMUX_AGENT_MTG_UNIT", "mtg-multi"),
			HealthURL: or("TELEMUX_AGENT_MTG_STATS_URL", "http://127.0.0.1:8081/stats"),
		}, nil
	}
	return Target{}, fmt.Errorf("продукт %q: нужен %s или %s", product, stack.ProductTelemt, stack.ProductMtg)
}

// ResolveBin - абсолютный путь бинаря: путь как есть, имя - через PATH (lookPath;
// nil - exec.LookPath).
func ResolveBin(bin string, lookPath func(string) (string, error)) (string, error) {
	if filepath.IsAbs(bin) {
		return bin, nil
	}
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	p, err := lookPath(bin)
	if err != nil {
		return "", fmt.Errorf("бинарь %s не найден: %w", bin, err)
	}
	return filepath.Abs(p)
}
