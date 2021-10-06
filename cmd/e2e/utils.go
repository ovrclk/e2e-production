package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/flags"
	"github.com/cosmos/cosmos-sdk/client/tx"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authclient "github.com/cosmos/cosmos-sdk/x/auth/client"
	dtypes "github.com/ovrclk/akash/x/deployment/types"

	"github.com/pkg/errors"
	"github.com/spf13/pflag"
	ttypes "github.com/tendermint/tendermint/types"
)


const (
	broadcastBlockRetryTimeout = 300 * time.Second
	broadcastBlockRetryPeriod  = time.Second

	// sadface.

	// Only way to detect the timeout error.
	// https://github.com/tendermint/tendermint/blob/46e06c97320bc61c4d98d3018f59d47ec69863c9/rpc/core/mempool.go#L124
	timeoutErrorMessage = "timed out waiting for tx to be included in a block"

	// Only way to check for tx not found error.
	// https://github.com/tendermint/tendermint/blob/46e06c97320bc61c4d98d3018f59d47ec69863c9/rpc/core/tx.go#L31-L33
	notFoundErrorMessageSuffix = ") not found"
)

// SendMsgs sends given sdk messages
func SendMsgs(ctx context.Context, cctx client.Context, flags *pflag.FlagSet, datagrams []sdk.Msg) (err error) {
	// validate basic all the msgs
	for _, msg := range datagrams {
		if err := msg.ValidateBasic(); err != nil {
			return err
		}
	}

	return BroadcastTX(ctx, cctx, flags, datagrams...)
}

// TxCreateDeployment takes DeploymentData and creates the specified deployment
func TxCreateDeployment(ctx context.Context, cctx client.Context, flags *pflag.FlagSet, dd *DeploymentData) (err error) {
	return SendMsgs(ctx, cctx, flags, []sdk.Msg{dd.MsgCreate()})
}

// TxCloseDeployment takes DeploymentData and closes the specified deployment
func TxCloseDeployment(ctx context.Context, cctx client.Context, flags *pflag.FlagSet, did dtypes.DeploymentID) (err error) {
	return SendMsgs(ctx, cctx, flags, []sdk.Msg{
		&dtypes.MsgCloseDeployment{
			ID: did,
		}})
}

func BroadcastTX(ctx context.Context, cctx client.Context, flags *pflag.FlagSet, msgs ...sdk.Msg) error {
	// rewrite of https://github.com/cosmos/cosmos-sdk/blob/ca98fda6eae597b1e7d468f96d030b6d905748d7/client/tx/tx.go#L29
	// to add continuing retries if broadcast-mode=block fails with a timeout.

	txf := tx.NewFactoryCLI(cctx, flags)

	if cctx.GenerateOnly {
		return tx.GenerateTx(cctx, txf, msgs...)
	}

	txf, err := tx.PrepareFactory(cctx, txf)
	if err != nil {
		return err
	}

	txf, err = adjustGas(cctx, txf, msgs...)
	if err != nil {
		return err
	}
	if cctx.Simulate {
		return nil
	}

	txb, err := tx.BuildUnsignedTx(txf, msgs...)
	if err != nil {
		return err
	}

	err = tx.Sign(txf, cctx.GetFromName(), txb, true)
	if err != nil {
		return err
	}

	txBytes, err := cctx.TxConfig.TxEncoder()(txb.GetTx())
	if err != nil {
		return err
	}

	res, err := doBroadcast(ctx, cctx, broadcastBlockRetryTimeout, txBytes)
	if err != nil {
		return err

	}

	return cctx.PrintProto(res)
}

func doBroadcast(ctx context.Context, cctx client.Context, timeout time.Duration, txb ttypes.Tx) (*sdk.TxResponse, error) {
	switch cctx.BroadcastMode {
	case flags.BroadcastSync:
		return cctx.BroadcastTxSync(txb)
	case flags.BroadcastAsync:
		return cctx.BroadcastTxAsync(txb)
	}

	// broadcast-mode=block

	// submit with mode commit/block
	cres, err := cctx.BroadcastTxCommit(txb)

	if err == nil {
		// no error, return
		return cres, nil
	} else if !strings.HasSuffix(err.Error(), timeoutErrorMessage) {
		// error other than timeout, return
		return cres, err
	} else if cres == nil {
		return cres, errors.Wrapf(err, "wtf")
	}

	// timeout error, continue on to retry

	// loop
	lctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for lctx.Err() == nil {
		// wait up to one second
		select {
		case <-lctx.Done():
			return cres, err
		case <-time.After(broadcastBlockRetryPeriod):
		}

		// check transaction
		res, err := authclient.QueryTx(cctx, cres.TxHash)
		if err == nil {
			return res, nil
		}

		// if it's not a "not found" error, return
		if !strings.HasSuffix(err.Error(), notFoundErrorMessageSuffix) {
			return res, err
		}
	}

	return cres, lctx.Err()
}

func adjustGas(ctx client.Context, txf tx.Factory, msgs ...sdk.Msg) (tx.Factory, error) {
	if !ctx.Simulate && !txf.SimulateAndExecute() {
		return txf, nil
	}
	_, adjusted, err := tx.CalculateGas(ctx.QueryWithData, txf, msgs...)
	if err != nil {
		return txf, err
	}

	txf = txf.WithGas(adjusted)
	_, _ = fmt.Fprintf(os.Stderr, "%s\n", tx.GasEstimateResponse{GasEstimate: txf.Gas()})

	return txf, nil
}
