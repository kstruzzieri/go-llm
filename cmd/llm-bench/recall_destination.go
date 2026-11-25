package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/kstruzzieri/go-llm/provider"
)

const (
	recallDefaultHost     = "127.0.0.1:8090"
	recallDefaultEndpoint = "http://" + recallDefaultHost
)

type recallDestinationKey struct{}

type recallDestinationBinding struct {
	destination provider.Destination
	client      *http.Client
}

func registeredRecallDestination(raw string) (provider.Destination, error) {
	if raw == "" {
		raw = recallDefaultEndpoint
	}
	dest, err := provider.NewDestination(openAICompatTransport, raw)
	if err != nil {
		return provider.Destination{}, fmt.Errorf("recall registration endpoint: %w", err)
	}
	if !dest.IsLocal() {
		return provider.Destination{}, errors.New("recall registration requires a loopback endpoint")
	}
	return dest, nil
}

func (b recallDestinationBinding) validate(actual string, targets []ModelTarget) error {
	for _, t := range targets {
		if t.Provider != openAICompatTransport {
			return errors.New("recall capture requires the openai-compat transport")
		}
	}
	return checkRecallEndpoint(b.destination, actual)
}

func checkRecallEndpoint(registered provider.Destination, actual string) error {
	dest, err := provider.NewDestination(openAICompatTransport, actual)
	if err != nil {
		return fmt.Errorf("recall capture endpoint: %w", err)
	}
	if dest != registered {
		return errors.New("recall capture endpoint differs from the registered loopback endpoint")
	}
	return nil
}

// bindRecallDestination gives the runner one guarded client for this capture.
// The guard disables redirects and proxies and pins localhost to loopback IPs.
func bindRecallDestination(ctx context.Context, registered, actual string, targets []ModelTarget) (context.Context, error) {
	dest, err := registeredRecallDestination(registered)
	if err != nil {
		return nil, err
	}
	binding := recallDestinationBinding{destination: dest}
	if err := binding.validate(actual, targets); err != nil {
		return nil, err
	}
	const purpose = "recall-capture"
	manifest, err := provider.NewDestinationManifest(provider.DestinationEdge{Purpose: purpose, Destination: dest})
	if err != nil {
		return nil, err
	}
	gate := provider.NewDestinationGate()
	if err := gate.Install(provider.DestinationPolicy{}, manifest); err != nil {
		return nil, err
	}
	ctx, err = gate.Bind(ctx, purpose, dest.Provider())
	if err != nil {
		return nil, err
	}
	binding.client, err = provider.GuardHTTPClient(gate, dest, nil)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, recallDestinationKey{}, binding), nil
}
