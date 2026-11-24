//go:build integration

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestIntegration_TrafficUsageWindowsAndDedup(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("pr1", "relay"), Target{
		Code: "pr2", Kind: "relay", Resources: []string{ResourceMtgConfig}, Enabled: true,
	})
	base := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	const key = "existing_123"
	report := func(minute int, node, product, generation string, apiOK, sample bool, in, out int64) RuntimeReport {
		at := base.Add(time.Duration(minute) * time.Minute)
		users := []RuntimeUser{{Key: key, BytesIn: in, BytesOut: out, TrafficSample: sample}}
		rep := RuntimeReport{Target: node, ObservedAt: &at}
		if product == ProductTelemt {
			rep.Telemt = &RuntimeTelemt{APIOK: apiOK, TrafficGeneration: generation, Users: users}
		} else {
			rep.Mtg = &RuntimeMtg{APIOK: apiOK, TrafficGeneration: generation, Users: users}
		}
		return rep
	}
	record := func(rep RuntimeReport) {
		t.Helper()
		if err := st.RecordRuntime(ctx, rep); err != nil {
			t.Fatal(err)
		}
	}
	usage := func(since, windowSince, until, freshSince int) TrafficUsage {
		t.Helper()
		minute := func(n int) time.Time { return base.Add(time.Duration(n) * time.Minute) }
		got, err := st.TrafficUsage(ctx, key, minute(since), minute(windowSince), minute(until), minute(freshSince))
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	empty := usage(1, 2, 3, 0)
	if empty.TotalBytes != "0" || empty.WindowBytes != "0" || empty.MeasuredAt != nil || empty.Complete {
		t.Fatalf("расход без показаний: %+v", empty)
	}
	record(report(0, "pr1", ProductTelemt, "systemd:1", true, true, 100, 200))
	record(report(0, "pr2", ProductMtg, "m1", true, true, 10, 20))
	delta := report(1, "pr1", ProductTelemt, "systemd:1", true, true, 110, 220)
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- st.RecordRuntime(ctx, delta) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	record(report(2, "pr2", ProductMtg, "m1", true, true, 15, 27))
	record(report(0, "pr1", ProductTelemt, "systemd:1", true, true, 1000, 1000))
	got := usage(1, 2, 3, 1)
	if got.TotalBytes != "42" || got.WindowBytes != "12" || !got.Complete ||
		got.MeasuredAt == nil || !got.MeasuredAt.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("сумма узлов, границы окна и дедуп: %+v", got)
	}
	if got := usage(1, 1, 2, 1); got.TotalBytes != "30" || got.WindowBytes != "30" {
		t.Fatalf("правая граница исключена: %+v", got)
	}

	// Пропуск не сбрасывает базу. Поколение mtg меняется даже при росте.
	record(report(3, "pr1", ProductTelemt, "", true, false, 0, 0))
	record(report(3, "pr2", ProductMtg, "m2", false, true, 0, 0))
	if got := usage(1, 2, 4, 1); got.TotalBytes != "42" || got.Complete {
		t.Fatalf("ошибка источника оставляет последний расход и снимает полноту: %+v", got)
	}
	record(report(4, "pr1", ProductTelemt, "", true, true, 3, 230))
	record(report(4, "pr2", ProductMtg, "m2", true, true, 18, 29))
	got = usage(1, 4, 5, 4)
	if got.TotalBytes != "102" || got.WindowBytes != "60" || got.Complete {
		t.Fatalf("сброс, пропуск и неполное покрытие: %+v", got)
	}
	got = usage(5, 5, 6, 3)
	if got.MeasuredAt != nil || got.Complete || got.TotalBytes != "0" {
		t.Fatalf("показания до активации: %+v", got)
	}
}

func TestIntegration_TrafficFreeFirstSampleAndResume(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, telemtTarget("pr1", "relay"))
	base := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	const key = "free_123"
	write := func(minute int, users ...RuntimeUser) {
		t.Helper()
		at := base.Add(time.Duration(minute) * time.Minute)
		rep := telemtRep("pr1", users...)
		rep.ObservedAt = &at
		rep.Telemt.TrafficGeneration = "systemd:1"
		if err := st.RecordRuntime(ctx, rep); err != nil {
			t.Fatal(err)
		}
	}
	check := func(minute int, total, window string, complete bool) {
		t.Helper()
		until := base.Add(time.Duration(minute)*time.Minute + time.Second)
		got, err := st.TrafficUsage(ctx, key, base, base.Add(4*time.Minute), until, until.Add(-3*time.Minute))
		if err != nil || got.TotalBytes != json.Number(total) || got.WindowBytes != json.Number(window) || got.Complete != complete {
			t.Fatalf("минута %d: ждали %s/%s, complete=%t, получили %+v (%v)", minute, total, window, complete, got, err)
		}
	}
	user := func(in, out int64) RuntimeUser {
		return RuntimeUser{Key: key, TrafficSample: true, BytesIn: in, BytesOut: out}
	}
	write(4, user(100, 150))
	write(4, user(100, 150))
	check(4, "250", "250", true)
	write(8)
	check(8, "250", "250", false)
	write(9, user(110, 155))
	check(9, "265", "265", true)
	write(13)
	check(13, "265", "265", false)
	write(14, user(2, 3))
	check(14, "270", "270", false)
	write(15, user(3, 5))
	check(15, "273", "273", true)
}

func TestIntegration_TrafficTwoMtgOnSameTarget(t *testing.T) {
	st := setupAll(t)
	defer st.Close()
	ctx := context.Background()
	seedTargets(t, st, Target{
		Code: "pr2", Kind: "relay", Resources: []string{ResourceMtgConfig, ResourceMtgSecondaryConfig}, Enabled: true,
	})
	base := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	write := func(minute int, primary, secondary string) {
		t.Helper()
		at := base.Add(time.Duration(minute) * time.Minute).Format(time.RFC3339)
		var rep RuntimeReport
		body := fmt.Sprintf(`{"target":"pr2","observed_at":%q,"mtg":%s,"mtg_secondary":%s}`, at, primary, secondary)
		if err := json.Unmarshal([]byte(body), &rep); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordRuntime(ctx, rep); err != nil {
			t.Fatal(err)
		}
	}
	source := func(generation string, in, out int) string {
		return fmt.Sprintf(`{"api_ok":true,"traffic_generation":%q,"users":[{"key":"free_123","traffic_sample":true,"bytes_in":%d,"bytes_out":%d}]}`, generation, in, out)
	}
	check := func(minute int, total, window json.Number, complete bool) {
		t.Helper()
		until := base.Add(time.Duration(minute)*time.Minute + time.Second)
		got, err := st.TrafficUsage(ctx, "free_123", base, base.Add(2*time.Minute), until, until.Add(-3*time.Minute))
		if err != nil || got.TotalBytes != total || got.WindowBytes != window || got.Complete != complete {
			t.Fatalf("минута %d: ждали %s/%s, complete=%t, получили %+v (%v)", minute, total, window, complete, got, err)
		}
	}
	write(1, source("g1", 10, 20), "null")
	check(1, "30", "0", false)
	write(2, source("g1", 13, 22), source("g1", 100, 200))
	write(2, source("g1", 13, 22), source("g1", 100, 200))
	check(2, "335", "305", true)
	write(3, source("g1", 16, 24), `{"api_ok":false}`)
	check(3, "340", "310", false)
	write(4, source("g1", 17, 25), source("g1", 110, 210))
	check(4, "362", "332", true)
}
