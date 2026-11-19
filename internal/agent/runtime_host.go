package agent

// Нагрузка машины из /proc (Linux): CPU и сеть - по разнице двух замеров
// (между отчётами), память, load, uptime - текущие, диск - statfs. На других ОС
// /proc нет - поля остаются нулевыми, отчёт всё равно уходит.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

type cpuSample struct{ idle, total uint64 }

type netSample struct {
	iface  string
	rx, tx uint64
	at     time.Time
}

// ParseCPUStat - суммарные idle (idle+iowait) и total из строки "cpu" /proc/stat.
func ParseCPUStat(data []byte) (cpuSample, error) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var s cpuSample
		for i, v := range f[1:] {
			if i >= 8 { // guest и guest_nice уже входят в user и nice
				break
			}
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return s, errors.New("/proc/stat: не число")
			}
			s.total += n
			if i == 3 || i == 4 { // idle, iowait
				s.idle += n
			}
		}
		return s, nil
	}
	return cpuSample{}, errors.New("/proc/stat: нет строки cpu")
}

// CPUPercent - загрузка CPU между двумя замерами, 0..100.
func CPUPercent(prev, cur cpuSample) float64 {
	if cur.total <= prev.total || cur.idle < prev.idle {
		return 0
	}
	dt := float64(cur.total - prev.total)
	busy := dt - float64(cur.idle-prev.idle)
	return round1(math.Max(0, math.Min(100, busy/dt*100)))
}

// ParseMemInfo - занятая память (MemTotal - MemAvailable), %.
func ParseMemInfo(data []byte) (float64, error) {
	var total, avail float64
	var haveAvail bool
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail, haveAvail = v, true
		}
	}
	if total <= 0 || !haveAvail {
		return 0, errors.New("/proc/meminfo: нет MemTotal или MemAvailable")
	}
	return round1((total - avail) / total * 100), nil
}

// firstFloat - первое число строки (/proc/loadavg, /proc/uptime).
func firstFloat(data []byte) (float64, error) {
	f := strings.Fields(string(data))
	if len(f) == 0 {
		return 0, errors.New("пусто")
	}
	return strconv.ParseFloat(f[0], 64)
}

// DefaultRouteIface - интерфейс маршрута по умолчанию из /proc/net/route.
func DefaultRouteIface(data []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 3 && f[1] == "00000000" && f[0] != "Iface" {
			return f[0]
		}
	}
	return ""
}

// ParseNetDev - принятые и отправленные байты интерфейса из /proc/net/dev.
func ParseNetDev(data []byte, iface string) (rx, tx uint64, ok bool) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		name, rest, found := strings.Cut(sc.Text(), ":")
		if !found || strings.TrimSpace(name) != iface {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			return 0, 0, false
		}
		rx, err1 := strconv.ParseUint(f[0], 10, 64)
		tx, err2 := strconv.ParseUint(f[8], 10, 64)
		return rx, tx, err1 == nil && err2 == nil
	}
	return 0, 0, false
}

// mbps - скорость в мегабитах по разнице счётчиков; сброс счётчика - 0.
func mbps(prev, cur uint64, dt time.Duration) float64 {
	if cur < prev || dt <= 0 {
		return 0
	}
	return round2(float64(cur-prev) * 8 / dt.Seconds() / 1e6)
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }

func (c *RuntimeCollector) readProc(name string) ([]byte, error) {
	return c.ReadFile(c.ProcRoot + "/" + name)
}

func (c *RuntimeCollector) cpuNow() (cpuSample, error) {
	data, err := c.readProc("stat")
	if err != nil {
		return cpuSample{}, err
	}
	return ParseCPUStat(data)
}

func (c *RuntimeCollector) collectHost(ctx context.Context) *store.RuntimeHost {
	h := &store.RuntimeHost{}
	if cur, err := c.cpuNow(); err == nil {
		prev := c.prevCPU
		if prev == nil {
			// Первый отчёт: второй замер через короткую паузу.
			select {
			case <-ctx.Done():
			case <-time.After(c.CPUSampleGap):
			}
			p := cur
			prev = &p
			if next, err := c.cpuNow(); err == nil {
				cur = next
			}
		}
		h.CPUPct = CPUPercent(*prev, cur)
		c.prevCPU = &cur
	}
	if data, err := c.readProc("meminfo"); err == nil {
		h.MemPct, _ = ParseMemInfo(data)
	}
	if data, err := c.readProc("loadavg"); err == nil {
		if v, err := firstFloat(data); err == nil {
			h.Load1 = round2(v)
		}
	}
	if data, err := c.readProc("uptime"); err == nil {
		if v, err := firstFloat(data); err == nil {
			h.UptimeS = int64(v)
		}
	}
	if pct, err := c.DiskUsage(c.DiskPath); err == nil {
		h.DiskPct = round1(pct)
	}
	c.collectNet(h)
	return h
}

func (c *RuntimeCollector) collectNet(h *store.RuntimeHost) {
	route, err := c.readProc("net/route")
	if err != nil {
		return
	}
	iface := DefaultRouteIface(route)
	if iface == "" {
		return
	}
	dev, err := c.readProc("net/dev")
	if err != nil {
		return
	}
	rx, tx, ok := ParseNetDev(dev, iface)
	if !ok {
		return
	}
	cur := &netSample{iface: iface, rx: rx, tx: tx, at: c.Now()}
	if p := c.prevNet; p != nil && p.iface == iface {
		dt := cur.at.Sub(p.at)
		h.NetRxMbps, h.NetTxMbps = mbps(p.rx, rx, dt), mbps(p.tx, tx, dt)
	}
	c.prevNet = cur
}
