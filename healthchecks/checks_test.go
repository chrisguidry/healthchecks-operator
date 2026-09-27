package healthchecks_test

import (
	"context"
	"testing"
	"time"

	"github.com/chrisguidry/healthchecks-operator/healthchecks"
)

func TestDeleteRemovesTheCheck(t *testing.T) {
	client, server := newTestClient(t)
	ctx := context.Background()

	result, err := client.Upsert(ctx, healthchecks.UpsertRequest{
		Slug:   "backup",
		Grace:  time.Minute,
		Period: healthchecks.FixedTimeout(time.Hour),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := client.Delete(ctx, result.UUID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := server.Check("backup"); ok {
		t.Error("server still has the check after Delete")
	}
}

func TestDeleteOfAnAlreadyGoneCheckIsSuccess(t *testing.T) {
	client, _ := newTestClient(t)
	if err := client.Delete(context.Background(), "00000000-0000-4000-8000-000000000000"); err != nil {
		t.Fatalf("Delete of a UUID the server never saw: %v", err)
	}
}

func TestFindBySlugReturnsTheMatchingCheck(t *testing.T) {
	client, _ := newTestClient(t)
	ctx := context.Background()

	created, err := client.Upsert(ctx, healthchecks.UpsertRequest{
		Slug:   "backup",
		Grace:  time.Minute,
		Period: healthchecks.FixedTimeout(time.Hour),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	found, err := client.FindBySlug(ctx, "backup")
	if err != nil {
		t.Fatalf("FindBySlug: %v", err)
	}
	if found == nil || found.UUID != created.UUID {
		t.Errorf("FindBySlug = %+v, want the check just created", found)
	}
}

func TestFindBySlugReturnsNilForAnUnknownSlug(t *testing.T) {
	client, _ := newTestClient(t)
	found, err := client.FindBySlug(context.Background(), "no-such-slug")
	if err != nil {
		t.Fatalf("FindBySlug: %v", err)
	}
	if found != nil {
		t.Errorf("FindBySlug = %+v, want nil", found)
	}
}
