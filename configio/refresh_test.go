package configio

import (
	"context"
	"errors"
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
		{Key: key("alpha", "m-a"), Family: "fa", Caps: provider.CapChat, KnownMask: provider.CapChat | provider.CapEmbed, ContextWindow: 8192},
		{Key: key("alpha", "m-b"), Family: "fb", Caps: provider.CapChat, KnownMask: provider.CapChat | provider.CapEmbed},
		{Key: key("zeta", "z-1"), Caps: provider.CapChat, KnownMask: provider.CapChat | provider.CapEmbed},
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

func TestRefreshInventory_UnreachableProviderIsInValue(t *testing.T) {
	lister := &fakeLister{
		names: []string{"down", "up"},
		providers: map[string]provider.Provider{
			"down": &fakeProvider{name: "down", modelsErr: errors.New("connection refused")},
			"up":   &fakeProvider{name: "up", models: []provider.ModelInfo{{Name: "m"}}},
		},
	}
	inv, err := RefreshInventory(context.Background(), lister, newFakeProjector())
	if err != nil {
		t.Fatalf("RefreshInventory() error: %v; a down provider must not abort the refresh", err)
	}
	wantProviders := []configview.InventoryProvider{
		{Name: "down", Reachable: false},
		{Name: "up", Reachable: true},
	}
	if !reflect.DeepEqual(inv.Providers, wantProviders) {
		t.Fatalf("Providers = %+v; want %+v", inv.Providers, wantProviders)
	}
	if len(inv.Models) != 1 || inv.Models[0].Key != key("up", "m") {
		t.Fatalf("Models = %+v; want exactly the reachable provider's model", inv.Models)
	}
}

func TestRefreshInventory_PreCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	inv, err := RefreshInventory(ctx, &fakeLister{names: []string{"p"}}, newFakeProjector())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled", err)
	}
	if !reflect.DeepEqual(inv, configview.Inventory{}) {
		t.Fatalf("pre-cancelled refresh published %+v; want zero Inventory", inv)
	}
}

func TestRefreshInventory_CancelDuringOnlyProviderListing(t *testing.T) {
	// SINGLE provider: no later loop-entry check can mask a missing
	// error-branch barrier. Without the error-branch ctx check the
	// provider would be recorded unreachable and the refresh would
	// SUCCEED with a published value.
	ctx, cancel := context.WithCancel(context.Background())
	lister := &fakeLister{
		names: []string{"only"},
		providers: map[string]provider.Provider{
			"only": &fakeProvider{name: "only", onModels: cancel, modelsErr: context.Canceled},
		},
	}
	inv, err := RefreshInventory(ctx, lister, newFakeProjector())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled", err)
	}
	if !reflect.DeepEqual(inv, configview.Inventory{}) {
		t.Fatalf("cancelled refresh published %+v; want zero Inventory", inv)
	}
}

func TestRefreshInventory_CancelAfterLastProviderSucceeds(t *testing.T) {
	// Cancellation lands AFTER the only provider's listing and projection
	// both succeed — only the FINAL barrier stands between this and a
	// published value.
	ctx, cancel := context.WithCancel(context.Background())
	proj := newFakeProjector()
	proj.onCall = func(context.Context) { cancel() }
	lister := &fakeLister{
		names: []string{"only"},
		providers: map[string]provider.Provider{
			"only": &fakeProvider{name: "only", models: []provider.ModelInfo{{Name: "m"}}},
		},
	}
	inv, err := RefreshInventory(ctx, lister, proj)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled (final barrier)", err)
	}
	if !reflect.DeepEqual(inv, configview.Inventory{}) {
		t.Fatalf("cancelled refresh published %+v; want zero Inventory", inv)
	}
}

func TestRefreshInventory_ProjectionCancellationAborts(t *testing.T) {
	// The projection's only error is cancellation; refresh must return it
	// verbatim with the zero value — and must NOT record the provider
	// unreachable (the listing succeeded; Reachable reports the provider).
	ctx, cancel := context.WithCancel(context.Background())
	proj := newFakeProjector()
	proj.onCall = func(context.Context) { cancel() }
	proj.err = context.Canceled
	lister := &fakeLister{
		names: []string{"only"},
		providers: map[string]provider.Provider{
			"only": &fakeProvider{name: "only", models: []provider.ModelInfo{{Name: "m"}}},
		},
	}
	inv, err := RefreshInventory(ctx, lister, proj)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled", err)
	}
	if !reflect.DeepEqual(inv, configview.Inventory{}) {
		t.Fatalf("cancelled refresh published %+v; want zero Inventory", inv)
	}
}

func TestRefreshInventory_VanishedProviderIsUnreachable(t *testing.T) {
	// Named by the lister but not resolvable via Get (racing unregister):
	// recorded unreachable, refresh proceeds.
	lister := &fakeLister{
		names:     []string{"ghost", "real"},
		providers: map[string]provider.Provider{"real": &fakeProvider{name: "real", models: []provider.ModelInfo{{Name: "m"}}}},
	}
	inv, err := RefreshInventory(context.Background(), lister, newFakeProjector())
	if err != nil {
		t.Fatalf("RefreshInventory() error: %v", err)
	}
	wantProviders := []configview.InventoryProvider{
		{Name: "ghost", Reachable: false},
		{Name: "real", Reachable: true},
	}
	if !reflect.DeepEqual(inv.Providers, wantProviders) {
		t.Fatalf("Providers = %+v; want %+v", inv.Providers, wantProviders)
	}
	if len(inv.Models) != 1 || inv.Models[0].Key != key("real", "m") {
		t.Fatalf("Models = %+v; want exactly the resolvable provider's model", inv.Models)
	}
}
