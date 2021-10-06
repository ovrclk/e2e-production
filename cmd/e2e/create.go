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
	"github.com/tendermint/tendermint/libs/log"

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

func (wfb *waitForBids) run(ctx context.Context, cctx client.Context, flags *pflag.FlagSet) error {
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
			return ctx.Err()
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
	err := SendMsgs(ctx, cctx, flags, mcr)
	if err != nil {
		return err
	}

	return nil
}

func newWaitForLeases(dd *DeploymentData, gClientDir *gateway.ClientDirectory, retryConfiguration []retry.Option, leasesReady <-chan struct{}) *waitForLeases {
	return &waitForLeases{
		log:                logger.With("cli", "lease checker"),
		dd:                 dd,
		gClientDir:         gClientDir,
		leasesReady:        leasesReady,
		done:               make(chan error, 1),
		retryConfiguration: retryConfiguration,
	}
}

type leaseAndService struct {
	leaseID     mtypes.LeaseID
	serviceName string
}

type waitForLeases struct {
	log                log.Logger
	dd                 *DeploymentData
	gClientDir         *gateway.ClientDirectory
	leasesReady        <-chan struct{}
	done               chan error
	retryConfiguration []retry.Option
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
func (wfl *waitForLeases) run(ctx context.Context) {
	var err error

	defer func() {
		if err != nil {
			wfl.log.Error("Waiting on leases to be ready", "error", err.Error())
		} else {
			wfl.log.Info("All leases are ready")
		}

		wfl.done <- err
	}()

	// Wait for signal that expected leases exist
	select {
	case <-wfl.leasesReady:
	case <-ctx.Done():
		err = ctx.Err()
		return
	}

	leases := wfl.dd.Leases()
	wfl.log.Info("Waiting on leases to be ready", "leaseQuantity", len(leases))

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

	checks := make([]func() error, 0, len(leases))

	for _, leaseID := range leases {
		var fn func() error

		fn, err = wfl.leaseChecker(ctx, leaseID, localRetryConfiguration...)
		if err != nil {
			return
		}

		checks = append(checks, fn)
	}

	group, _ := errgroup.WithContext(ctx)

	for _, fn := range checks {
		group.Go(fn)
	}

	err = group.Wait()

	return
}

func (wfl *waitForLeases) leaseChecker(ctx context.Context, leaseID mtypes.LeaseID, retryOpts ...retry.Option) (func() error, error) {
	wfl.log.Debug("Checking status of lease", "lease", leaseID)

	gclient, err := wfl.gClientDir.GetClientFromBech32(leaseID.GetProvider())
	if err != nil {
		return nil, err
	}

	servicesChecked := make(map[string]bool)

	return func() error {
		err = retry.Do(func() error {
			ls, err := gclient.LeaseStatus(ctx, leaseID)

			if err != nil {
				wfl.log.Debug("Could not get lease status", "lease", leaseID, "err", err)
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
					wfl.log.Debug(err.Error())

					return err
				}

				for _, u := range s.URIs {
					req, err := http.NewRequest("GET", "http://"+u, nil)
					if err != nil {
						wfl.log.Debug(err.Error())
						return err
					}

					req = req.WithContext(ctx)

					res, err := http.DefaultClient.Do(req)
					if err != nil {
						wfl.log.Debug(err.Error())
						return err
					}

					if res.StatusCode != http.StatusOK {
						err = fmt.Errorf("service's http endpoint returned error: %s", res.Status)
						wfl.log.Debug(err.Error())
						return httpStatus(res.StatusCode)
					}
				}
				servicesChecked[serviceName] = true
				wfl.log.Info("service ready", "lease", leaseID, "service", serviceName)
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
		}, retryOpts...)
		if err != nil {
			return err
		}

		wfl.log.Info("lease ready", "leaseID", leaseID)
		return nil
	}, nil
}
