package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/avast/retry-go"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/flags"
	"github.com/cosmos/cosmos-sdk/server"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/ovrclk/akash/app"
	"github.com/ovrclk/akash/cmd/common"
	ctypes "github.com/ovrclk/akash/provider/cluster/types"
	gateway "github.com/ovrclk/akash/provider/gateway/rest"
	"github.com/ovrclk/akash/sdkutil"
	cutils "github.com/ovrclk/akash/x/cert/utils"
	dcli "github.com/ovrclk/akash/x/deployment/client/cli"
	mtypes "github.com/ovrclk/akash/x/market/types"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"github.com/tendermint/tendermint/libs/log"
	"golang.org/x/sync/errgroup"

	tmcfg "github.com/tendermint/tendermint/config"
	tmcli "github.com/tendermint/tendermint/libs/cli"
)

var (
	// logger is the logger for the application
	logger = log.NewTMLogger(log.NewSyncWriter(os.Stdout))
)

// Execute executes the root command.
func Execute(rootCmd *cobra.Command) error {
	// Create and set a client.Context on the command's Context. During the pre-run
	// of the root command, a default initialized client.Context is provided to
	// seed child command execution with values such as AccountRetriever, Keyring,
	// and a Tendermint RPC. This requires the use of a pointer reference when
	// getting and setting the client.Context. Ideally, we utilize
	// https://github.com/spf13/cobra/pull/1118.
	srvCtx := server.NewDefaultContext()
	ctx := context.Background()
	ctx = context.WithValue(ctx, client.ClientContextKey, &client.Context{})
	ctx = context.WithValue(ctx, server.ServerContextKey, srvCtx)

	rootCmd.PersistentFlags().String(flags.FlagLogLevel, zerolog.InfoLevel.String(), "The logging level (trace|debug|info|warn|error|fatal|panic)")
	rootCmd.PersistentFlags().String(flags.FlagLogFormat, tmcfg.LogFormatPlain, "The logging format (json|plain)")

	executor := tmcli.PrepareBaseCmd(rootCmd, "AKASH", app.DefaultHome)
	return executor.ExecuteContext(ctx)
}

// RootCmd represents root command of deploy tool
func main() {
	sdkutil.InitSDKConfig()

	encodingConfig := app.MakeEncodingConfig()
	initClientCtx := client.Context{}.
		WithJSONMarshaler(encodingConfig.Marshaler).
		WithInterfaceRegistry(encodingConfig.InterfaceRegistry).
		WithTxConfig(encodingConfig.TxConfig).
		WithLegacyAmino(encodingConfig.Amino).
		WithInput(os.Stdin).
		WithAccountRetriever(authtypes.AccountRetriever{}).
		WithBroadcastMode(flags.BroadcastBlock).
		WithHomeDir(app.DefaultHome)

	cmd := &cobra.Command{
		Use:          "e2e [sdl-file]",
		Short:        "Akash e2e tool commands",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := server.InterceptConfigsPreRunHandler(cmd); err != nil {
				return err
			}

			return client.SetCmdClientContextHandler(initClientCtx, cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			timeoutDuration, err := cmd.Flags().GetDuration(FlagTimeout)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithDeadline(cmd.Context(), time.Now().Add(timeoutDuration))
			defer cancel()

			tickDuration, err := cmd.Flags().GetDuration(FlagTick)
			if err != nil {
				return err
			}

			maxDelay := tickDuration
			const defaultMaxDelay = 15 * time.Second
			if maxDelay < defaultMaxDelay {
				maxDelay = defaultMaxDelay
			}

			retryConfiguration := []retry.Option{
				retry.DelayType(retry.BackOffDelay),
				retry.Attempts(9999), // Use a large number here, since a deadline is used on the context
				retry.MaxDelay(maxDelay),
				retry.Delay(tickDuration),
				retry.RetryIf(retryIfGatewayClientResponseError),
				retry.Context(ctx),
			}

			clientCtx, err := client.GetClientTxContext(cmd)
			if err != nil {
				return err
			}

			if _, err = cutils.LoadAndQueryPEMForAccount(cmd.Context(), clientCtx, clientCtx.Keyring); err != nil {
				if os.IsNotExist(err) {
					err = errors.Errorf("no certificate file found for account %q.\n"+
						"consider creating it as certificate required to create a deployment", clientCtx.FromAddress.String())
				}

				return err
			}

			gClientDir, err := gateway.NewClientDirectory(clientCtx)
			if err != nil {
				return err
			}

			log := logger.With("cli", "create")
			dd, err := NewDeploymentData(args[0], cmd.Flags(), clientCtx)
			if err != nil {
				return err
			}

			group, _ := errgroup.WithContext(ctx)

			// Listen to on chain events and send the manifest when required
			leasesReady := make(chan struct{}, 1)
			bids := make(chan mtypes.EventBidCreated, 1)
			group.Go(func() error {
				err := ChainEmitter(
					ctx,
					clientCtx,
					DeploymentDataUpdateHandler(dd, bids, leasesReady),
					SendManifestHandler(clientCtx, dd, gClientDir, retryConfiguration))
				if err != nil && !errors.Is(err, context.Canceled) {
					log.Error("error watching events", "err", err)
					cancel()
				}
				return err
			})

			depCreated := make(chan error, 1)
			depTeardown := make(chan struct{}, 1)

			// Send the deployment creation transaction
			group.Go(func() error {
				if err := TxCreateDeployment(clientCtx, cmd.Flags(), dd); err != nil && !errors.Is(err, context.Canceled) {
					log.Error("error creating deployment", "err", err)
					cancel()
				}

				depCreated <- err
				return err
			})

			defer func() {
				depTeardown <- struct{}{}
				<- depCreated
			}()

			go func() {
				if e := <-depCreated; e != nil {
					return
				}

				<-depTeardown

				log.Info("closing deployment", "dseq", dd.DeploymentID)
				if e := TxCloseDeployment(clientCtx, cmd.Flags(), dd); e != nil {
					log.Error("closing deployment", "dseq", dd.DeploymentID, "err", e)
				}

				depCreated <- nil
			}()

			wfb := newWaitForBids(dd, bids)
			group.Go(func() error {
				if err := wfb.run(ctx, cancel, clientCtx, cmd.Flags()); err != nil && !errors.Is(err, context.Canceled) {
					log.Error("error waiting for bids to be made", "err", err)
					cancel()
				}
				return err
			})

			wfl := newWaitForLeases(dd, gClientDir, retryConfiguration, leasesReady)
			// Wait for the leases to be created and then start polling the provider for service availability
			group.Go(func() error {
				if err := wfl.run(ctx, cancel); err != nil && !errors.Is(err, context.Canceled) {
					log.Error("error waiting for services to be ready", "err", err)
					cancel()
				}
				return err
			})

			// This returns "context cancelled" when everything goes OK
			err = group.Wait()
			cancel()
			if err != nil && errors.Is(err, context.Canceled) && wfl.allLeasesOk {
				err = nil // Not an actual error to stop on
			}

			if err != nil {
				return err
			}

			// Reset the context
			ctx, cancel2 := context.WithDeadline(cmd.Context(), time.Now().Add(timeoutDuration))
			err = wfl.eachService(func(leaseID mtypes.LeaseID, serviceName string) error {
				gclient, err := gClientDir.GetClientFromBech32(leaseID.Provider)
				if err != nil {
					return err
				}

				var status *ctypes.ServiceStatus
				if err = retry.Do(func() error {
					status, err = gclient.ServiceStatus(ctx, leaseID, serviceName)
					return err
				}); err != nil {
					return err
				}

				// Encode and show the response
				statusEncoded, err := json.MarshalIndent(status, "", " ")
				if err != nil {
					return nil
				}

				_, err = os.Stdout.Write(statusEncoded)
				if err != nil {
					return err
				}
				_, err = fmt.Print("\n")
				return err
			})
			cancel2()

			if errors.Is(err, context.Canceled) {
				return errDeployTimeout
			}

			return err
		},
	}

	cmd.PersistentFlags().String(flags.FlagChainID, "", "The network chain ID")
	cmd.PersistentFlags().String(flags.FlagNode, "tcp://rpc0.mainnet.akash.network:26657", "The node address")
	cmd.Flags().String(flags.FlagChainID, "", "The network chain ID")
	cmd.Flags().Duration(FlagTimeout, 300*time.Second, "The max amount of time to wait for deployment status checking process")
	cmd.Flags().Duration(FlagTick, 500*time.Millisecond, "The time interval at which deployment status is checked")

	cmd.PersistentFlags().String(flags.FlagFrom, "", "name or address of private key with which to sign")
	if err := cmd.MarkPersistentFlagRequired(flags.FlagFrom); err != nil {
		panic(err.Error())
	}

	flags.AddTxFlagsToCmd(cmd)
	dcli.AddDeploymentIDFlags(cmd.Flags())
	common.AddDepositFlags(cmd.Flags(), DefaultDeposit)

	if err := Execute(cmd); err != nil {
		switch e := err.(type) {
		case server.ErrorCode:
			os.Exit(e.Code)
		default:
			os.Exit(1)
		}
	}

	// cmd.AddCommand(createCmd())
}
