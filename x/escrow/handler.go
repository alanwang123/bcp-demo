package escrow

import (
	"github.com/confio/weave"
	"github.com/confio/weave/errors"
	"github.com/confio/weave/orm"
	"github.com/confio/weave/x"
	"github.com/confio/weave/x/cash"
)

const (
	// pay escrow cost up-front
	createEscrowCost  int64 = 300
	returnEscrowCost  int64 = 0
	releaseEscrowCost int64 = 0
	updateEscrowCost  int64 = 50
)

// RegisterRoutes will instantiate and register
// all handlers in this package
func RegisterRoutes(r weave.Registry, auth x.Authenticator,
	control cash.Controller) {

	bucket := NewBucket()
	r.Handle(pathCreateEscrowMsg, CreateEscrowHandler{auth, bucket, control})
	r.Handle(pathReleaseEscrowMsg, ReleaseEscrowHandler{auth, bucket, control})
	r.Handle(pathReturnEscrowMsg, ReturnEscrowHandler{auth, bucket, control})
	r.Handle(pathUpdateEscrowPartiesMsg, UpdateEscrowHandler{auth, bucket})
}

// RegisterQuery will register this bucket as "/wallets"
func RegisterQuery(qr weave.QueryRouter) {
	NewBucket().Register("escrows", qr)
}

//---- create

// CreateEscrowHandler will set a name for objects in this bucket
type CreateEscrowHandler struct {
	auth   x.Authenticator
	bucket Bucket
	cash   cash.Controller
}

var _ weave.Handler = CreateEscrowHandler{}

// Check just verifies it is properly formed and returns
// the cost of executing it
func (h CreateEscrowHandler) Check(ctx weave.Context, db weave.KVStore,
	tx weave.Tx) (weave.CheckResult, error) {
	var res weave.CheckResult
	_, err := h.validate(ctx, db, tx)
	if err != nil {
		return res, err
	}

	// return cost
	res.GasAllocated += createEscrowCost
	return res, nil
}

// Deliver moves the tokens from sender to receiver if
// all preconditions are met
func (h CreateEscrowHandler) Deliver(ctx weave.Context, db weave.KVStore,
	tx weave.Tx) (weave.DeliverResult, error) {
	var res weave.DeliverResult
	msg, err := h.validate(ctx, db, tx)
	if err != nil {
		return res, err
	}

	// apply a default for sender
	sender := weave.Permission(msg.Sender)
	if sender == nil {
		sender = x.MainSigner(ctx, h.auth)
	}

	// create an escrow object
	escrow := &Escrow{
		Sender:    sender,
		Arbiter:   msg.Arbiter,
		Recipient: msg.Recipient,
		Amount:    msg.Amount,
		Timeout:   msg.Timeout,
		Memo:      msg.Memo,
	}
	obj, err := h.bucket.Create(db, escrow)
	if err != nil {
		return res, err
	}

	// move the money to this object
	dest := Permission(obj.Key()).Address()
	sendAddr := sender.Address()
	for _, c := range escrow.Amount {
		err := h.cash.MoveCoins(db, sendAddr, dest, *c)
		if err != nil {
			// this will rollback the half-finished tx
			return res, err
		}
	}

	// return id of escrow to use in future calls
	res.Data = obj.Key()
	return res, err
}

// validate does all common pre-processing between Check and Deliver
func (h CreateEscrowHandler) validate(ctx weave.Context, db weave.KVStore,
	tx weave.Tx) (*CreateEscrowMsg, error) {

	rmsg, err := tx.GetMsg()
	if err != nil {
		return nil, err
	}
	msg, ok := rmsg.(*CreateEscrowMsg)
	if !ok {
		return nil, errors.ErrUnknownTxType(rmsg)
	}

	err = msg.Validate()
	if err != nil {
		return nil, err
	}

	// verify that timeout is in the future
	height, _ := weave.GetHeight(ctx)
	if msg.Timeout <= height {
		return nil, ErrInvalidTimeout(msg.Timeout)
	}

	// sender must authorize this (if not set, defaults to MainSigner)
	if msg.Sender != nil {
		sender := weave.Permission(msg.Sender).Address()
		if !h.auth.HasAddress(ctx, sender) {
			return nil, errors.ErrUnauthorized()
		}
	}

	// TODO: check balance? or just error on deliver?

	return msg, nil
}

//---- release

// ReleaseEscrowHandler will set a name for objects in this bucket
type ReleaseEscrowHandler struct {
	auth   x.Authenticator
	bucket Bucket
	cash   cash.Controller
}

var _ weave.Handler = ReleaseEscrowHandler{}

// Check just verifies it is properly formed and returns
// the cost of executing it
func (h ReleaseEscrowHandler) Check(ctx weave.Context, db weave.KVStore,
	tx weave.Tx) (weave.CheckResult, error) {
	var res weave.CheckResult
	_, _, err := h.validate(ctx, db, tx)
	if err != nil {
		return res, err
	}

	// return cost
	res.GasAllocated += releaseEscrowCost
	return res, nil
}

// Deliver moves the tokens from sender to receiver if
// all preconditions are met
func (h ReleaseEscrowHandler) Deliver(ctx weave.Context, db weave.KVStore,
	tx weave.Tx) (weave.DeliverResult, error) {
	var res weave.DeliverResult
	msg, obj, err := h.validate(ctx, db, tx)
	if err != nil {
		return res, err
	}
	escrow := AsEscrow(obj)

	// use amount in message, or
	request := x.Coins(msg.Amount)
	available := x.Coins(escrow.Amount)
	if len(request) == 0 {
		request = available

		// TODO: add functionality to compare two sets
		// } else if !available.Contains(request) {
		// 	// ensure there is enough to pay
		// 	return res, cash.ErrInsufficientFunds()
	}

	// move the money from escrow to recipient
	sender := Permission(obj.Key()).Address()
	dest := weave.Permission(escrow.Recipient).Address()
	for _, c := range request {
		err := h.cash.MoveCoins(db, sender, dest, *c)
		if err != nil {
			// this will rollback the half-finished tx
			return res, err
		}
		// remove coin from remaining balance
		available, err = available.Subtract(*c)
		if err != nil {
			return res, err
		}
	}

	// if there is something left, just update the balance...
	if available.IsPositive() {
		// return id as we can use again
		res.Data = obj.Key()
		// this updates the object, as we have a pointer
		escrow.Amount = available
		err = h.bucket.Save(db, obj)
	} else {
		// otherwise we finished the escrow and can delete it
		err = h.bucket.Delete(db, obj.Key())
	}

	// returns error if Save/Delete failed
	return res, err
}

// validate does all common pre-processing between Check and Deliver
func (h ReleaseEscrowHandler) validate(ctx weave.Context, db weave.KVStore,
	tx weave.Tx) (*ReleaseEscrowMsg, orm.Object, error) {

	rmsg, err := tx.GetMsg()
	if err != nil {
		return nil, nil, err
	}
	msg, ok := rmsg.(*ReleaseEscrowMsg)
	if !ok {
		return nil, nil, errors.ErrUnknownTxType(rmsg)
	}

	err = msg.Validate()
	if err != nil {
		return nil, nil, err
	}

	// load escrow
	obj, err := h.bucket.Get(db, msg.EscrowId)
	if err != nil {
		return nil, nil, err
	}
	escrow := AsEscrow(obj)
	if escrow == nil {
		return nil, nil, ErrNoSuchEscrow(msg.EscrowId)
	}

	// arbiter must authorize this
	arbiter := weave.Permission(escrow.Arbiter).Address()
	if !h.auth.HasAddress(ctx, arbiter) {
		return nil, nil, errors.ErrUnauthorized()
	}

	// timeout must not have expired
	height, _ := weave.GetHeight(ctx)
	if escrow.Timeout < height {
		return nil, nil, ErrEscrowExpired(escrow.Timeout)
	}

	return msg, obj, nil
}

//---- return

// ReturnEscrowHandler will set a name for objects in this bucket
type ReturnEscrowHandler struct {
	auth   x.Authenticator
	bucket Bucket
	cash   cash.Controller
}

var _ weave.Handler = ReturnEscrowHandler{}

// Check just verifies it is properly formed and returns
// the cost of executing it
func (h ReturnEscrowHandler) Check(ctx weave.Context, db weave.KVStore,
	tx weave.Tx) (weave.CheckResult, error) {
	var res weave.CheckResult
	_, err := h.validate(ctx, db, tx)
	if err != nil {
		return res, err
	}

	// return cost
	res.GasAllocated += returnEscrowCost
	return res, nil
}

// Deliver moves the tokens from sender to receiver if
// all preconditions are met
func (h ReturnEscrowHandler) Deliver(ctx weave.Context, db weave.KVStore,
	tx weave.Tx) (weave.DeliverResult, error) {
	var res weave.DeliverResult
	obj, err := h.validate(ctx, db, tx)
	if err != nil {
		return res, err
	}
	escrow := AsEscrow(obj)

	// move the money from escrow to recipient
	sender := Permission(obj.Key()).Address()
	dest := weave.Permission(escrow.Sender).Address()
	for _, c := range escrow.Amount {
		err := h.cash.MoveCoins(db, sender, dest, *c)
		if err != nil {
			// this will rollback the half-finished tx
			return res, err
		}
	}

	// now remove the finished escrow
	err = h.bucket.Delete(db, obj.Key())

	// returns error if Delete failed
	return res, err
}

// validate does all common pre-processing between Check and Deliver
func (h ReturnEscrowHandler) validate(ctx weave.Context, db weave.KVStore,
	tx weave.Tx) (orm.Object, error) {

	rmsg, err := tx.GetMsg()
	if err != nil {
		return nil, err
	}
	msg, ok := rmsg.(*ReturnEscrowMsg)
	if !ok {
		return nil, errors.ErrUnknownTxType(rmsg)
	}

	err = msg.Validate()
	if err != nil {
		return nil, err
	}

	// load escrow
	obj, err := h.bucket.Get(db, msg.EscrowId)
	if err != nil {
		return nil, err
	}
	escrow := AsEscrow(obj)
	if escrow == nil {
		return nil, ErrNoSuchEscrow(msg.EscrowId)
	}

	// timeout must have expired
	height, _ := weave.GetHeight(ctx)
	if height <= escrow.Timeout {
		return nil, ErrEscrowNotExpired(escrow.Timeout)
	}

	return obj, nil
}

//---- update

// UpdateEscrowHandler will set a name for objects in this bucket
type UpdateEscrowHandler struct {
	auth   x.Authenticator
	bucket Bucket
}

var _ weave.Handler = UpdateEscrowHandler{}

// Check just verifies it is properly formed and returns
// the cost of executing it
func (h UpdateEscrowHandler) Check(ctx weave.Context, db weave.KVStore,
	tx weave.Tx) (weave.CheckResult, error) {
	var res weave.CheckResult
	_, _, err := h.validate(ctx, db, tx)
	if err != nil {
		return res, err
	}

	// return cost
	res.GasAllocated += updateEscrowCost
	return res, nil
}

// Deliver moves the tokens from sender to receiver if
// all preconditions are met
func (h UpdateEscrowHandler) Deliver(ctx weave.Context, db weave.KVStore,
	tx weave.Tx) (weave.DeliverResult, error) {
	var res weave.DeliverResult
	msg, obj, err := h.validate(ctx, db, tx)
	if err != nil {
		return res, err
	}
	escrow := AsEscrow(obj)

	// update the escrow with message values
	if msg.Sender != nil {
		escrow.Sender = msg.Sender
	}
	if msg.Recipient != nil {
		escrow.Recipient = msg.Recipient
	}
	if msg.Arbiter != nil {
		escrow.Arbiter = msg.Arbiter
	}

	// save the updated escrow
	err = h.bucket.Save(db, obj)

	// returns error if Save failed
	return res, err
}

// validate does all common pre-processing between Check and Deliver
func (h UpdateEscrowHandler) validate(ctx weave.Context, db weave.KVStore,
	tx weave.Tx) (*UpdateEscrowPartiesMsg, orm.Object, error) {

	rmsg, err := tx.GetMsg()
	if err != nil {
		return nil, nil, err
	}
	msg, ok := rmsg.(*UpdateEscrowPartiesMsg)
	if !ok {
		return nil, nil, errors.ErrUnknownTxType(rmsg)
	}

	err = msg.Validate()
	if err != nil {
		return nil, nil, err
	}

	// load escrow
	obj, err := h.bucket.Get(db, msg.EscrowId)
	if err != nil {
		return nil, nil, err
	}
	escrow := AsEscrow(obj)
	if escrow == nil {
		return nil, nil, ErrNoSuchEscrow(msg.EscrowId)
	}

	// timeout must not have expired
	height, _ := weave.GetHeight(ctx)
	if height > escrow.Timeout {
		return nil, nil, ErrEscrowExpired(escrow.Timeout)
	}

	// we must have the permission for the items we want to change
	if msg.Sender != nil {
		sender := weave.Permission(escrow.Sender).Address()
		if !h.auth.HasAddress(ctx, sender) {
			return nil, nil, errors.ErrUnauthorized()
		}
	}
	if msg.Recipient != nil {
		rcpt := weave.Permission(escrow.Recipient).Address()
		if !h.auth.HasAddress(ctx, rcpt) {
			return nil, nil, errors.ErrUnauthorized()
		}
	}
	if msg.Arbiter != nil {
		arbiter := weave.Permission(escrow.Arbiter).Address()
		if !h.auth.HasAddress(ctx, arbiter) {
			return nil, nil, errors.ErrUnauthorized()
		}
	}

	return msg, obj, nil
}
