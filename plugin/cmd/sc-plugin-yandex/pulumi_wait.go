// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	_ "unsafe" // for go:linkname

	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// pulumiContextWait is the Pulumi Go SDK's (*Context).wait: it blocks until
// every goroutine the context started has finished — the RegisterResource
// RPCs, which run asynchronously and only fire once their input Outputs
// resolve, and the Apply callbacks — then refuses further RPCs and returns the
// first RPC error. pulumi.RunWithContext calls it before a program exits.
//
// The plugin cannot use RunWithContext: that registers a pulumi:pulumi:Stack
// resource, and the plugin's context writes into SC core's stack, which already
// has one. A context made with sdk.NewContext has no exported way to drain it,
// and without the drain a handler that returns and closes its context drops
// every registration still waiting on an Output: the yc-serverless-container
// invoker binding (its ContainerId is container.ID()) never reached the engine
// in two-peaks run 40954213000, so the container answered 403 to everyone.
//
// The SDK version is pinned in go.mod; a rename of this method fails the link,
// not silently at runtime.
//
//go:linkname pulumiContextWait github.com/pulumi/pulumi/sdk/v3/go/pulumi.(*Context).wait
func pulumiContextWait(ctx *sdk.Context) error

// finishPulumiContext waits for all of pctx's outstanding work, then closes
// it, and returns the wait's error. A Close error stays ignored, as it was
// before the wait existed: by then every RPC has completed.
func finishPulumiContext(pctx *sdk.Context) error {
	err := pulumiContextWait(pctx)
	_ = pctx.Close()
	return err
}

// settle runs a handler's deferred context teardown and folds its error into
// the handler's result: the RPC's response is only sent once every
// registration the handler started has reached the engine, and a registration
// that failed fails the RPC instead of being lost. A handler error already set
// takes precedence.
func settle[T any](done func() error, resp **T, err *error) {
	if doneErr := done(); doneErr != nil && *err == nil {
		*resp = nil
		*err = doneErr
	}
}
