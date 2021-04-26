package main

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"sync"
	"time"

	dtypes "github.com/ovrclk/akash/x/deployment/types"
	mtypes "github.com/ovrclk/akash/x/market/types"

	"github.com/avast/retry-go"
	"github.com/cosmos/cosmos-sdk/client"
	sdk "github.com/cosmos/cosmos-sdk/types"
	gateway "github.com/ovrclk/akash/provider/gateway/rest"
	"github.com/pkg/errors"
	"github.com/spf13/pflag"
	"golang.org/x/sync/errgroup"
)

const (
	// FlagTimeout represents max amount of time for lease status checking process
	FlagTimeout = "timeout"
	// FlagTick represents time interval at which lease status is checked
	FlagTick = "tick"
)

type httpStatus int

func (e httpStatus) Error() string {
	return http.StatusText(int(e))
}

func retryIfGatewayClientResponseError(err error) bool {
	_, isClientErr := err.(gateway.ClientResponseError)
	return isClientErr
}

func retryIfHTTPLeaseResponseError(err error) bool {
	_, isErr := err.(httpStatus)
	return isErr
}

var errDeployTimeout = errors.New("Timed out while trying to deploy")
var DefaultDeposit = dtypes.DefaultDeploymentMinDeposit

func newWaitForBids(dd *DeploymentData, bids <-chan mtypes.EventBidCreated) *waitForBids {
	return &waitForBids{
		bids:       bids,
		groupCount: len(dd.Groups),
	}
}

type waitForBids struct {
	groupCount int
	bids       <-chan mtypes.EventBidCreated
}

type bidList []mtypes.EventBidCreated

func (bl bidList) Len() int {
	return len(bl)
}

func (bl bidList) Less(i, j int) bool {
	lhs := bl[i]
	rhs := bl[j]

	if !lhs.Price.Amount.Equal(rhs.Price.Amount) {
		return lhs.Price.Amount.LT(rhs.Price.Amount)
	}

	return lhs.ID.Provider < rhs.ID.Provider
}

func (bl bidList) Swap(i, j int) {
	bl[i], bl[j] = bl[j], bl[i]
}

func (wfb *waitForBids) run(ctx context.Context, cancel context.CancelFunc, clientCtx client.Context, flags *pflag.FlagSet) error {
	allBids := make(map[uint32]bidList)

	tickerStarted := false
	ticker := time.NewTicker(15 * time.Second)
	ticker.Stop()

	defer func() {
		ticker.Stop()
	}()

loop:
	for {
		select {
		case bid := <-wfb.bids:
			logger.Debug("Processing bid")
			allBids[bid.ID.GSeq] = append(allBids[bid.ID.GSeq], bid)

			// If there is a bid for at least every group, then start the deadline
			if !tickerStarted && len(allBids) == wfb.groupCount {
				logger.Debug("All groups have at least one bid")
				// TODO - this value was made up
				ticker.Reset(time.Second * 15)
				tickerStarted = true
			}

		case <-ctx.Done():
			cancel()
			return context.Canceled
		case <-ticker.C:
			logger.Info("Done waiting on bids", "qty", len(allBids))
			break loop
		}
	}

	var mcr []sdk.Msg

	for gseq, bidsForGroup := range allBids {
		// Create the lease, using the lowest price
		sort.Sort(bidsForGroup)
		winningBid := bidsForGroup[0]

		// check for more than 1 bid having the same price
		if len(bidsForGroup) > 1 && winningBid.Price.Equal(bidsForGroup[1].Price) {
			identical := make(bidList, 0)
			for _, bid := range bidsForGroup {
				if bid.Price.Equal(winningBid.Price) {
					identical = append(identical, bid)
				}
			}
			logger.Info("Multiple bids with identical price", "gseq", gseq, "price", winningBid.Price.String(), "qty", len(identical))
			rng := rand.New(rand.NewSource(int64(winningBid.ID.DSeq))) // nolint
			choice := rng.Intn(len(identical))

			winningBid = identical[choice]
		}

		logger.Info("Winning bid", "gseq", gseq, "price", winningBid.Price.String(), "provider", winningBid.ID.Provider)

		mcr = append(mcr, &mtypes.MsgCreateLease{
			BidID: winningBid.ID,
		})
	}
	resp, err := SendMsgs(clientCtx, flags, mcr)
	if err != nil {
		return err
	}

	log := logger.With(
		"hash", resp.TxHash,
		"code", resp.Code,
		"codespace", resp.Codespace,
		"action", "create-lease(s)",
	)

	for i, m := range mcr {
		log = log.With(fmt.Sprintf("lid%d", i), m.(*mtypes.MsgCreateLease).BidID.LeaseID())
	}

	log.Info("tx sent successfully")

	return nil
}

func newWaitForLeases(dd *DeploymentData, gClientDir *gateway.ClientDirectory, retryConfiguration []retry.Option, leasesReady <-chan struct{}) *waitForLeases {
	return &waitForLeases{
		dd:                 dd,
		gClientDir:         gClientDir,
		leasesReady:        leasesReady,
		retryConfiguration: retryConfiguration,
		allLeasesOk:        false,
	}
}

type leaseAndService struct {
	leaseID     mtypes.LeaseID
	serviceName string
}

type waitForLeases struct {
	dd                 *DeploymentData
	gClientDir         *gateway.ClientDirectory
	leasesReady        <-chan struct{}
	retryConfiguration []retry.Option
	allLeasesOk        bool
	services           []leaseAndService
	lock               sync.Mutex
}

func (wfl *waitForLeases) eachService(fn func(leaseID mtypes.LeaseID, serviceName string) error) error {
	for _, entry := range wfl.services {
		err := fn(entry.leaseID, entry.serviceName)
		if err != nil {
			return err
		}
	}
	return nil
}

var errLeaseNotReady = errors.New("lease not ready")

// WaitForLeasesAndPollService waits for leases
func (wfl *waitForLeases) run(ctx context.Context, cancel context.CancelFunc) error {
	log := logger

	// Wait for signal that expected leases exist
	select {
	case <-wfl.leasesReady:

	case <-ctx.Done():
		cancel()
		return context.Canceled
	}

	leases := wfl.dd.Leases()
	log.Info("Waiting on leases to be ready", "leaseQuantity", len(leases))

	var localRetryConfiguration []retry.Option
	localRetryConfiguration = append(localRetryConfiguration, wfl.retryConfiguration...)

	retryIf := func(err error) bool {
		if retryIfGatewayClientResponseError(err) {
			return true
		}

		if retryIfHTTPLeaseResponseError(err) {
			return true
		}

		return errors.Is(err, errLeaseNotReady)
	}
	localRetryConfiguration = append(localRetryConfiguration, retry.RetryIf(retryIf))

	leaseChecker := func(leaseID mtypes.LeaseID) (func() error, error) {
		log.Debug("Checking status of lease", "lease", leaseID)

		gclient, err := wfl.gClientDir.GetClientFromBech32(leaseID.GetProvider())
		if err != nil {
			cancel()
			return nil, err
		}

		servicesChecked := make(map[string]bool)

		return func() error {
			err = retry.Do(func() error {
				ls, err := gclient.LeaseStatus(ctx, leaseID)

				if err != nil {
					log.Debug("Could not get lease status", "lease", leaseID, "err", err)
					return err
				}

				for serviceName, s := range ls.Services {
					checked := servicesChecked[serviceName]
					if checked {
						continue
					}
					isOk := s.Available == s.Total
					if !isOk {
						err = fmt.Errorf("%w: service %q has %d / %d available", errLeaseNotReady, serviceName, s.Available, s.Total)
						log.Debug(err.Error())

						return err
					}

					for _, u := range s.URIs {
						req, err := http.NewRequest("GET", "http://"+u, nil)
						if err != nil {
							log.Debug(err.Error())
							return err
						}

						req = req.WithContext(ctx)

						res, err := http.DefaultClient.Do(req)
						if err != nil {
							log.Debug(err.Error())
							return err
						}

						if res.StatusCode != http.StatusOK {
							err = fmt.Errorf("service's http endpoint returned error: %s", res.Status)
							log.Debug(err.Error())
							return httpStatus(res.StatusCode)
						}
					}
					servicesChecked[serviceName] = true
					log.Info("service ready", "lease", leaseID, "service", serviceName)
				}

				// Update the shared data
				wfl.lock.Lock()
				defer wfl.lock.Unlock()
				for serviceName := range ls.Services {
					wfl.services = append(wfl.services, leaseAndService{
						leaseID:     leaseID,
						serviceName: serviceName,
					})
				}
				return nil
			}, localRetryConfiguration...)
			if err != nil {
				return err
			}

			log.Info("lease ready", "leaseID", leaseID)
			return nil
		}, nil
	}

	group, _ := errgroup.WithContext(ctx)

	for _, leaseID := range leases {
		fn, err := leaseChecker(leaseID)
		if err != nil {
			return err
		}
		group.Go(fn)
	}

	err := group.Wait()
	if err == nil { // If all return without error, then all leases are ready
		wfl.allLeasesOk = true
	}
	cancel()
	return nil
}

// TxCreateDeployment takes DeploymentData and creates the specified deployment
func TxCreateDeployment(clientCtx client.Context, flags *pflag.FlagSet, dd *DeploymentData) (err error) {
	res, err := SendMsgs(clientCtx, flags, []sdk.Msg{dd.MsgCreate()})
	log := logger.With(
		"msg", "create-deployment",
	)

	if err != nil || res == nil || res.Code != 0 {
		log.Error("tx failed")
		return err
	}

	log = logger.With(
		"hash", res.TxHash,
		"code", res.Code,
		"codespace", res.Codespace,
		"action", "create-deployment",
		"dseq", dd.DeploymentID.DSeq,
	)

	log.Info("tx sent successfully")
	return nil
}

// TxCloseDeployment takes DeploymentData and closes the specified deployment
func TxCloseDeployment(clientCtx client.Context, flags *pflag.FlagSet, dd *DeploymentData) (err error) {
	res, err := SendMsgs(clientCtx, flags, []sdk.Msg{dd.MsgClose()})
	log := logger.With(
		"msg", "close-deployment",
	)

	if err != nil || res == nil || res.Code != 0 {
		log.Error("tx failed")
		return err
	}

	log = logger.With(
		"hash", res.TxHash,
		"code", res.Code,
		"codespace", res.Codespace,
		"action", "close-deployment",
		"dseq", dd.DeploymentID.DSeq,
	)

	log.Info("tx sent successfully")
	return nil
}
