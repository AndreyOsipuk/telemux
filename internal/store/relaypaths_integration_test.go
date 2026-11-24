//go:build integration

package store

import (
	"context"
	"testing"
)

func TestIntegration_RelayMasterTargetsUsesEnabledAssignedExits(t *testing.T) {
	st := setupOps(t)
	ctx := context.Background()

	testRoutes := func(wantManaged bool, wantExit string) {
		t.Helper()
		managed, exits, err := st.RelayMasterTargets(ctx, "pr1")
		if err != nil || managed != wantManaged {
			t.Fatalf("managed=%v, err=%v", managed, err)
		}
		if wantExit == "" && len(exits) != 0 ||
			wantExit != "" && (len(exits) != 1 || exits[0].Code != wantExit) {
			t.Fatalf("список выходов: %+v, ждали %q", exits, wantExit)
		}
	}
	testRoutes(false, "")
	if _, err := st.pool.Exec(ctx, `UPDATE targets SET settings=settings||'{"relay_paths_managed":"1"}' WHERE code='pr1'`); err != nil {
		t.Fatal(err)
	}
	testRoutes(true, "ps1")
	if _, err := st.pool.Exec(ctx, `UPDATE targets SET enabled=false WHERE code='ps1'`); err != nil {
		t.Fatal(err)
	}
	testRoutes(true, "")
}
