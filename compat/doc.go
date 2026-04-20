// Package compat exposes provider.Router over an OpenAI-compatible HTTP REST
// API. Consumers such as Cursor, Continue.dev, Aider, and llama.cpp clients
// can point at a compat.Server instance and use it as a drop-in OpenAI
// backend. Request/response wire format matches OpenAI; go-llm-specific
// metadata is carried on x_ prefixed extension fields that standard OpenAI
// SDKs ignore.
//
// Usage:
//
//	router := provider.NewRouter(modelReg, provReg)
//	srv := compat.New(router, modelReg, provReg,
//	    compat.WithAddr("127.0.0.1:18741"),
//	    compat.WithMaxConcurrency(8),
//	)
//	defer srv.Close()
//	go srv.ListenAndServe(ctx)
package compat
