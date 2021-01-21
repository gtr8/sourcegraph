package db

import (
	"context"
	"testing"

	"github.com/sourcegraph/sourcegraph/internal/db/dbtest"
	"github.com/sourcegraph/sourcegraph/internal/types"
)

func TestUserPublicRepos_Set(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	db := dbtest.NewDB(t, "")
	ctx := context.Background()

	user, err := NewUserStoreWithDB(db).Create(ctx, NewUser{
		Username: "u",
		Password: "p",
	})
	if err != nil {
		t.Errorf("Expected no error, got %s ", err)
	}

	repoStore := NewRepoStoreWithDB(db)

	err = repoStore.Create(ctx, &types.Repo{
		Name: "test",
	})
	if err != nil {
		t.Errorf("Expected no error, got %s", err)
	}

	repo, err := repoStore.GetByName(ctx, "test")
	if err != nil {
		t.Errorf("Expected no error, got %s", err)
	}

	upStore := NewUserPublicRepoStoreWithDB(db)
	err = upStore.SetUserRepo(ctx, user.ID, repo.ID)
	if err != nil {
		t.Errorf("Expected no error, got %s", err)
	}

	repoIDs, err := upStore.ListByUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("Expected no error, got %s", err)
	}
	if wanted, got := 1, len(repoIDs); wanted != got {
		t.Errorf("wanted %v repos, got %v", wanted, got)
	}
	if wanted, got := int32(repo.ID), repoIDs[0]; wanted != got {
		t.Errorf("wanted repo ID %v, got %v", wanted, got)
	}
}