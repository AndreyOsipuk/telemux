package main

// Серверы (ADR 0005): импорт описания из JSON - массива ServerWrite, тот же
// формат, что у PUT /api/v1/servers.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/role"
	"github.com/AndreyOsipuk/telemux/internal/store"
)

// runServers: telemux servers import [--db dsn] <file.json>
func runServers(args []string) int {
	if len(args) == 0 || args[0] != "import" {
		fmt.Fprintln(os.Stderr, "использование: telemux servers import [--db dsn] <file.json|->")
		return 2
	}
	fs := flag.NewFlagSet("servers import", flag.ContinueOnError)
	dsn := fs.String("db", envOr("DATABASE_URL", ""), "DSN primary PG")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "нужен путь к JSON-файлу (массив серверов) или - для stdin")
		return 2
	}
	items, err := readServersFile(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	st, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "PG:", err)
		return 1
	}
	defer st.Close()
	if err := st.CheckServersSchema(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	r, err := role.Detect(ctx, st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "роль: %v\n", err)
		return 1
	}
	if !r.IsMaster() {
		fmt.Fprintln(os.Stderr, "это replica (PG read-only); импорт делается на master")
		return 1
	}
	ins, upd, err := st.ImportServers(ctx, items)
	if err != nil {
		fmt.Fprintf(os.Stderr, "импорт серверов (ничего не изменено): %v\n", err)
		return 1
	}
	fmt.Printf("серверы: новых=%d обновлено=%d (всего в файле=%d)\n", ins, upd, len(items))
	return 0
}

// readServersFile читает массив ServerWrite из файла или stdin ("-").
func readServersFile(path string) ([]store.ServerWrite, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("чтение %s: %w", path, err)
	}
	var items []store.ServerWrite
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("%s: нужен JSON-массив серверов: %w", path, err)
	}
	return items, nil
}
