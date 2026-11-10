package db

import (
	"context"
	"testing"
)

func TestBroadcastStorePersistsCloudAuthority(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()
	for _, cloud := range []bool{false, true} {
		b := newTestBroadcast()
		b.IsCloudControlled = cloud
		if err := store.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
		got, err := store.Get(ctx, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.IsCloudControlled != cloud {
			t.Fatalf("cloud authority=%v want=%v", got.IsCloudControlled, cloud)
		}
	}
}
