package configio

import (
	"context"
	"reflect"
	"testing"

	"github.com/kstruzzieri/go-llm/configview"
	"github.com/kstruzzieri/go-llm/provider"
)

func key(prov, model string) provider.ModelKey {
	return provider.ModelKey{Provider: prov, Model: model}
}

func TestRefreshInventory_HappyPathSortedAndComplete(t *testing.T) {
	// Providers listed in non-alphabetical order, models listed unsorted:
	// output must come out fully sorted. Literal expected values.
	lister := &fakeLister{
		names: []string{"zeta", "alpha"}, // deliberately unsorted
		providers: map[string]provider.Provider{
			"alpha": &fakeProvider{name: "alpha", models: []provider.ModelInfo{{Name: "m-b", Family: "fb"}, {Name: "m-a", Family: "fa", ContextWindow: 8192}}},
			"zeta":  &fakeProvider{name: "zeta", models: []provider.ModelInfo{{Name: "z-1"}}},
		},
	}
	proj := newFakeProjector()

	inv, err := RefreshInventory(context.Background(), lister, proj)
	if err != nil {
		t.Fatalf("RefreshInventory() error: %v", err)
	}

	wantProviders := []configview.InventoryProvider{
		{Name: "alpha", Reachable: true},
		{Name: "zeta", Reachable: true},
	}
	if !reflect.DeepEqual(inv.Providers, wantProviders) {
		t.Fatalf("Providers = %+v; want %+v", inv.Providers, wantProviders)
	}
	wantModels := []configview.InventoryModel{
		{Key: key("alpha", "m-a"), Family: "fa", Caps: provider.CapChat, KnownMask: provider.CapChat, ContextWindow: 8192},
		{Key: key("alpha", "m-b"), Family: "fb", Caps: provider.CapChat, KnownMask: provider.CapChat},
		{Key: key("zeta", "z-1"), Caps: provider.CapChat, KnownMask: provider.CapChat},
	}
	if !reflect.DeepEqual(inv.Models, wantModels) {
		t.Fatalf("Models = %+v; want %+v", inv.Models, wantModels)
	}

	// The projector received the SORTED listing.
	seen := proj.seenFor("alpha")
	if len(seen) != 2 || seen[0].Name != "m-a" || seen[1].Name != "m-b" {
		t.Fatalf("projector saw %+v; want sorted [m-a m-b]", seen)
	}
}

func TestRefreshInventory_Deterministic(t *testing.T) {
	lister := &fakeLister{
		names: []string{"b", "a"},
		providers: map[string]provider.Provider{
			"a": &fakeProvider{name: "a", models: []provider.ModelInfo{{Name: "y"}, {Name: "x"}}},
			"b": &fakeProvider{name: "b", models: []provider.ModelInfo{{Name: "q"}}},
		},
	}
	first, err := RefreshInventory(context.Background(), lister, newFakeProjector())
	if err != nil {
		t.Fatalf("first refresh error: %v", err)
	}
	second, err := RefreshInventory(context.Background(), lister, newFakeProjector())
	if err != nil {
		t.Fatalf("second refresh error: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two refreshes over identical state differ:\nfirst  = %+v\nsecond = %+v", first, second)
	}
}

func TestRefreshInventory_EmptyProviderSet(t *testing.T) {
	inv, err := RefreshInventory(context.Background(), &fakeLister{}, newFakeProjector())
	if err != nil {
		t.Fatalf("RefreshInventory() error: %v", err)
	}
	if len(inv.Models) != 0 || len(inv.Providers) != 0 {
		t.Fatalf("expected zero inventory, got %+v", inv)
	}
}
