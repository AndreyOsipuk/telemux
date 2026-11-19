package main

// Команды для ресурса telemt.config: текст ресурса из базы мастера и вывод
// машинной части (telemt.local.toml) при переводе машины на telemt.config.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/AndreyOsipuk/telemux/internal/store"
	"github.com/AndreyOsipuk/telemux/internal/telemtcfg"
)

// runDesired: telemux desired --target pr3 --resource telemt.config - последняя
// опубликованная ревизия ресурса машины в stdout.
func runDesired(args []string) int {
	fs := flag.NewFlagSet("desired", flag.ContinueOnError)
	dsn := fs.String("db", envOr("DATABASE_URL", ""), "DSN PG")
	target := fs.String("target", "", "код машины")
	resource := fs.String("resource", store.ResourceTelemtConfig, "ресурс")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *target == "" {
		fmt.Fprintln(os.Stderr, "нужен --target")
		return 2
	}
	ctx := context.Background()
	st, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "PG:", err)
		return 1
	}
	defer st.Close()
	d, err := st.LatestDesired(ctx, *target, *resource)
	if errors.Is(err, store.ErrNoDesired) {
		fmt.Fprintf(os.Stderr, "у %s нет опубликованного %s\n", *target, *resource)
		return 1
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "%s %s ревизия %d sha256 %s\n", *target, *resource, d.Revision, d.SHA256)
	_, _ = os.Stdout.Write(d.Content)
	return 0
}

// runTelemtLocal: telemux telemt-local --base base.toml [--current /etc/telemt/telemt.toml]
// печатает машинную часть - то, чем конфиг на машине отличается от telemt.config.
func runTelemtLocal(args []string) int {
	fs := flag.NewFlagSet("telemt-local", flag.ContinueOnError)
	base := fs.String("base", "", "текст telemt.config (telemux desired --target ... > base.toml)")
	current := fs.String("current", "/etc/telemt/telemt.toml", "telemt.toml на машине")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *base == "" {
		fmt.Fprintln(os.Stderr, "нужен --base")
		return 2
	}
	b, err := os.ReadFile(*base)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c, err := os.ReadFile(*current)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return telemtLocal(string(b), string(c), os.Stdout, os.Stderr)
}

// telemtLocal - машинная часть в out, предупреждения в errw. Код 1 - есть
// отличия, которые машинная часть выразить не может: сборка не совпадёт с машиной.
func telemtLocal(base, current string, out, errw io.Writer) int {
	overlay, warns := telemtcfg.DeriveOverlay(base, current)
	_, _ = io.WriteString(out, overlay)
	for _, w := range warns {
		fmt.Fprintln(errw, "внимание:", w)
	}
	if len(warns) > 0 {
		return 1
	}
	return 0
}
