// Command telemux — панель управления кластером telemt (MTProxy).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/role"
	syncpkg "github.com/AndreyOsipuk/telemux/internal/sync"
	"github.com/AndreyOsipuk/telemux/internal/store"
	"github.com/AndreyOsipuk/telemux/internal/telemt"
)

var version = "dev"

func main() {
	cmd := ""
	if len(os.Args) >= 2 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "probe":
		os.Exit(runProbe(os.Args[2:]))
	case "role":
		os.Exit(runRole(os.Args[2:]))
	case "sync":
		os.Exit(runSync(os.Args[2:]))
	case "-version", "--version", "version":
		fmt.Printf("telemux %s\n", version)
	default:
		fmt.Fprintln(os.Stderr, "telemux "+version)
		fmt.Fprintln(os.Stderr, "команды:")
		fmt.Fprintln(os.Stderr, "  probe --api <url> [--auth h]            — проверить связь с telemt-API")
		fmt.Fprintln(os.Stderr, "  role  --db <dsn>                        — роль ноды (master/replica) из локального PG")
		fmt.Fprintln(os.Stderr, "  sync  --db <dsn> --api <url> [--apply]  — синхронизировать локальный telemt (shadow по умолч.)")
	}
}

func runProbe(args []string) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	api := fs.String("api", "http://127.0.0.1:9091", "базовый URL telemt machine-API")
	auth := fs.String("auth", "", "значение заголовка Authorization")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c := telemt.New(*api, *auth)
	h, err := c.Health(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "health: ОШИБКА: %v\n", err)
		return 1
	}
	fmt.Printf("health: status=%s read_only=%v\n", h.Status, h.ReadOnly)
	users, rev, err := c.ListUsers(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "users: ОШИБКА: %v\n", err)
		return 1
	}
	fmt.Printf("users: всего=%d revision=%s\ntelemux видит ноду ✓\n", len(users), rev)
	return 0
}

func runRole(args []string) int {
	fs := flag.NewFlagSet("role", flag.ContinueOnError)
	dsn := fs.String("db", "", "DSN локального PG (postgres://...@127.0.0.1:5432/telemux)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "PG: %v\n", err)
		return 1
	}
	defer st.Close()
	r, err := role.Detect(ctx, st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	fmt.Printf("роль ноды: %s\n", r)
	return 0
}

func runSync(args []string) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	dsn := fs.String("db", "", "DSN локального PG")
	api := fs.String("api", "http://127.0.0.1:9091", "базовый URL telemt machine-API")
	auth := fs.String("auth", "", "значение заголовка Authorization")
	apply := fs.Bool("apply", false, "применять изменения (по умолчанию shadow — только показать)")
	force := fs.Bool("force", false, "обойти guard массового сноса")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	st, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "PG: %v\n", err)
		return 1
	}
	defer st.Close()

	mode := syncpkg.Shadow
	if *apply {
		mode = syncpkg.Apply
	}
	res, err := syncpkg.SyncNode(ctx, st, telemt.New(*api, *auth), syncpkg.Options{Mode: mode, Force: *force})
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync: %v\n", err)
		return 1
	}

	creates, patches, deletes := 0, 0, 0
	for _, op := range res.Ops {
		switch op.Kind {
		case "create":
			creates++
		case "patch":
			patches++
		case "delete":
			deletes++
		}
	}
	fmt.Printf("режим=%s diff: create=%d patch=%d delete=%d\n", mode, creates, patches, deletes)
	if res.Aborted {
		fmt.Println("⚠️  ЗАБЛОКИРОВАНО guard'ом массового сноса (нужен --force)")
		return 1
	}
	if mode == syncpkg.Apply {
		fmt.Printf("применено=%d пропущено(идемпот.)=%d ошибок=%d\n", res.Applied, res.Skipped, res.Failed)
		for _, e := range res.Errors {
			fmt.Fprintf(os.Stderr, "  ошибка: %v\n", e)
		}
		if res.Failed > 0 {
			return 1
		}
	}
	return 0
}
