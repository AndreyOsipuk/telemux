//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIntegration_UserCRUD(t *testing.T) {
	st := setup(t) // Open + миграция + TRUNCATE users
	defer st.Close()
	ctx := context.Background()

	// create (secret сгенерится)
	u, err := st.CreateUser(ctx, "sub_crud", "", nil, intp(8))
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Secret) != 32 || u.Username != "sub_crud" || !u.Enabled {
		t.Fatalf("create вернул кривого юзера: %+v", u)
	}
	// дубль → ErrUserExists
	if _, err := st.CreateUser(ctx, "sub_crud", "", nil, nil); !errors.Is(err, ErrUserExists) {
		t.Fatalf("дубль должен дать ErrUserExists, получили %v", err)
	}

	// в desired (enabled) попадает
	des, _ := st.ListDesired(ctx)
	if len(des) != 1 {
		t.Fatalf("desired ждали 1, получили %d", len(des))
	}

	// disable → из desired исчезает
	if ok, _ := st.SetEnabled(ctx, "sub_crud", false); !ok {
		t.Fatal("SetEnabled")
	}
	if des, _ := st.ListDesired(ctx); len(des) != 0 {
		t.Fatal("disabled не должен быть в desired")
	}

	// renew (срок)
	exp := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if ok, _ := st.SetExpiration(ctx, "sub_crud", &exp); !ok {
		t.Fatal("SetExpiration")
	}

	// list page
	list, total, err := st.ListUsersPage(ctx, 20, 0)
	if err != nil || total != 1 || len(list) != 1 || list[0].ExpirationAt == nil {
		t.Fatalf("ListUsersPage: total=%d len=%d err=%v", total, len(list), err)
	}

	// delete
	if ok, _ := st.DeleteUser(ctx, "sub_crud"); !ok {
		t.Fatal("delete")
	}
	if _, total, _ := st.ListUsersPage(ctx, 20, 0); total != 0 {
		t.Fatal("после delete total=0")
	}
}

func intp(i int) *int { return &i }
