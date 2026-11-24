package service

import (
	"maps"
	"testing"
)

func TestRetainExtraHonorsCountAndAllocatedBudget(t *testing.T) {
	sizes := map[string]uint64{"current": 10, "previous": 5, "newest": 7, "large": 50, "older": 3}
	updates := &imageUpdates{
		cacheMax: 25, extraRetained: 2,
		imageBytes: func(id string) (uint64, error) { return sizes[id], nil },
	}
	got := updates.retainExtra([]string{"older", "large", "newest", "previous"}, "current", "previous", "current")
	want := map[string]bool{"newest": true, "older": true}
	if !maps.Equal(got, want) {
		t.Fatalf("retained %v, want %v", got, want)
	}
}
