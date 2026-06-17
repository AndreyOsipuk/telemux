//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

func setupNodes(t *testing.T) *Store {
	t.Helper()
	st := setup(t) // из store_integration_test.go: Open + миграция + TRUNCATE users
	if _, err := st.pool.Exec(context.Background(), "TRUNCATE nodes; TRUNCATE join_tokens"); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestIntegration_NodesUpsertList(t *testing.T) {
	st := setupNodes(t)
	defer st.Close()
	ctx := context.Background()

	if err := st.UpsertNode(ctx, Node{Code: "ps1", Address: "10.0.0.1", Role: "master"}); err != nil {
		t.Fatal(err)
	}
	// повторный upsert по тому же code → обновление, не дубль
	if err := st.UpsertNode(ctx, Node{Code: "ps1", Address: "10.0.0.9", Role: "replica"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertNode(ctx, Node{Code: "ps2", Address: "10.0.0.2", Role: "replica"}); err != nil {
		t.Fatal(err)
	}
	nodes, err := st.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("ждали 2 ноды (ps1 обновлён, не задвоен), получили %d", len(nodes))
	}
	// ORDER BY code → ps1 первый, с обновлённым адресом/ролью + last_seen
	if nodes[0].Code != "ps1" || nodes[0].Address != "10.0.0.9" || nodes[0].Role != "replica" {
		t.Fatalf("ps1 не обновлён upsert'ом: %+v", nodes[0])
	}
	if nodes[0].LastSeenAt == nil {
		t.Fatal("last_seen_at должен проставиться")
	}
}

func TestIntegration_JoinTokenLifecycle(t *testing.T) {
	st := setupNodes(t)
	defer st.Close()
	ctx := context.Background()

	if err := st.CreateJoinToken(ctx, "tok-valid", time.Hour); err != nil {
		t.Fatalf("create token: %v", err) // ловит баг с интервалом
	}
	ok, err := st.ConsumeJoinToken(ctx, "tok-valid")
	if err != nil || !ok {
		t.Fatalf("первый consume должен быть успешен: ok=%v err=%v", ok, err)
	}
	ok, _ = st.ConsumeJoinToken(ctx, "tok-valid")
	if ok {
		t.Fatal("повторный consume должен быть false (одноразовый)")
	}
	ok, _ = st.ConsumeJoinToken(ctx, "несуществующий")
	if ok {
		t.Fatal("несуществующий токен → false")
	}

	// истёкший токен (отрицательный ttl) не консьюмится
	if err := st.CreateJoinToken(ctx, "tok-expired", -time.Hour); err != nil {
		t.Fatal(err)
	}
	ok, _ = st.ConsumeJoinToken(ctx, "tok-expired")
	if ok {
		t.Fatal("истёкший токен не должен консьюмиться")
	}
}
