/*
Package std contains standard implementations of a number
of components.

It is a good place to get started buuilding your first app,
and to see how to wire together the various components.
You can then replace them with custom implementations,
as your project grows.
*/
package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/confio/weave"
	"github.com/confio/weave/app"
	"github.com/confio/weave/store/iavl"
	"github.com/confio/weave/x"
	"github.com/confio/weave/x/sigs"
	"github.com/confio/weave/x/utils"

	"github.com/iov-one/bov-core/x/namecoin"
)

// Authenticator returns the typical authentication,
// just using public key signatures
func Authenticator() x.Authenticator {
	return sigs.Authenticate{}
}

// Chain returns a chain of decorators, to handle authentication,
// fees, logging, and recovery
func Chain(minFee x.Coin, authFn x.Authenticator) app.Decorators {
	return app.ChainDecorators(
		utils.NewLogging(),
		utils.NewRecovery(),
		// on CheckTx, bad tx don't affect state
		utils.NewSavepoint().OnCheck(),
		sigs.NewDecorator(),
		namecoin.NewFeeDecorator(authFn, minFee),
		// on DeliverTx, bad tx will increment nonce and take fee
		// even if the message fails
		utils.NewSavepoint().OnDeliver(),
	)
}

// Router returns a default router, only dispatching to the
// cash.SendMsg
func Router(authFn x.Authenticator, issuer weave.Address) app.Router {
	r := app.NewRouter()
	namecoin.RegisterRoutes(r, authFn, issuer)
	return r
}

// Stack wires up a standard router with a standard decorator
// chain. This can be passed into BaseApp.
func Stack(minFee x.Coin, issuer weave.Address) weave.Handler {
	authFn := Authenticator()
	return Chain(minFee, authFn).
		WithHandler(Router(authFn, issuer))
}

// Application constructs a basic ABCI application with
// the given arguments. If you are not sure what to use
// for the Handler, just use Stack().
func Application(name string, h weave.Handler,
	tx weave.TxDecoder, dbPath string) (app.BaseApp, error) {

	ctx := context.Background()
	kv, err := CommitKVStore(dbPath)
	if err != nil {
		return app.BaseApp{}, err
	}
	store := app.NewStoreApp(name, kv, ctx)
	base := app.NewBaseApp(store, tx, h, nil)
	return base, nil
}

// CommitKVStore returns an initialized KVStore that persists
// the data to the named path.
func CommitKVStore(dbPath string) (weave.CommitKVStore, error) {
	// memory backed case, just for testing
	if dbPath == "" {
		return iavl.MockCommitStore(), nil
	}

	// Expand the path fully
	path, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("Invalid Database Name: %s", path)
	}

	// Some external calls accidently add a ".db", which is now removed
	path = strings.TrimSuffix(path, filepath.Ext(path))

	// Split the database name into it's components (dir, name)
	dir := filepath.Dir(path)
	name := filepath.Base(path)
	return iavl.NewCommitStore(dir, name), nil
}