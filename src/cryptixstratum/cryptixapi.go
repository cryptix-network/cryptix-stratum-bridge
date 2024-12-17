package cryptixstratum

import (
	"context"
	"fmt"
	"time"

	"github.com/cryptix-network/cryptix-stratum-bridge/src/gostratum"
	"github.com/cryptix-network/cryptixd/app/appmessage"
	"github.com/cryptix-network/cryptixd/infrastructure/network/rpcclient"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type CryptixApi struct {
	address       string
	blockWaitTime time.Duration
	logger        *zap.SugaredLogger
	cryptixd      *rpcclient.RPCClient
	connected     bool
}

func NewCryptixAPI(address string, blockWaitTime time.Duration, logger *zap.SugaredLogger) (*CryptixApi, error) {
	client, err := rpcclient.NewRPCClient(address)
	if err != nil {
		return nil, err
	}

	return &CryptixApi{
		address:       address,
		blockWaitTime: blockWaitTime,
		logger:        logger.With(zap.String("component", "cryptixapi:"+address)),
		cryptixd:      client,
		connected:     true,
	}, nil
}

func (cytxApi *CryptixApi) Start(ctx context.Context, blockCb func()) {
	cytxApi.waitForSync(true)
	go cytxApi.startBlockTemplateListener(ctx, blockCb)
	go cytxApi.startStatsThread(ctx)
}

func (cytxApi *CryptixApi) startStatsThread(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			cytxApi.logger.Warn("context cancelled, stopping stats thread")
			return
		case <-ticker.C:
			dagResponse, err := cytxApi.cryptixd.GetBlockDAGInfo()
			if err != nil {
				cytxApi.logger.Warn("failed to get network hashrate from cryptix, prom stats will be out of date", zap.Error(err))
				continue
			}
			response, err := cytxApi.cryptixd.EstimateNetworkHashesPerSecond(dagResponse.TipHashes[0], 1000)
			if err != nil {
				cytxApi.logger.Warn("failed to get network hashrate from cryptix, prom stats will be out of date", zap.Error(err))
				continue
			}
			RecordNetworkStats(response.NetworkHashesPerSecond, dagResponse.BlockCount, dagResponse.Difficulty)
		}
	}
}

func (cytxApi *CryptixApi) reconnect() error {
	if cytxApi.cryptixd != nil {
		return cytxApi.cryptixd.Reconnect()
	}

	client, err := rpcclient.NewRPCClient(cytxApi.address)
	if err != nil {
		return err
	}
	cytxApi.cryptixd = client
	return nil
}

func (cytxApi *CryptixApi) waitForSync(verbose bool) error {
	if verbose {
		cytxApi.logger.Info("checking cryptixd sync state")
	}
	for {
		clientInfo, err := cytxApi.cryptixd.GetInfo()
		if err != nil {
			return errors.Wrapf(err, "error fetching server info from cryptixd @ %s", cytxApi.address)
		}
		if clientInfo.IsSynced {
			break
		}
		cytxApi.logger.Warn("Cryptix is not synced, waiting for sync before starting bridge")
		time.Sleep(5 * time.Second)
	}
	if verbose {
		cytxApi.logger.Info("cryptixd synced, starting server")
	}
	return nil
}

func (cytxApi *CryptixApi) startBlockTemplateListener(ctx context.Context, blockReadyCb func()) {
	blockReadyChan := make(chan bool)
	err := cytxApi.cryptixd.RegisterForNewBlockTemplateNotifications(func(_ *appmessage.NewBlockTemplateNotificationMessage) {
		blockReadyChan <- true
	})
	if err != nil {
		cytxApi.logger.Error("fatal: failed to register for block notifications from cryptix")
	}

	ticker := time.NewTicker(cytxApi.blockWaitTime)
	for {
		if err := cytxApi.waitForSync(false); err != nil {
			cytxApi.logger.Error("error checking cryptixd sync state, attempting reconnect: ", err)
			if err := cytxApi.reconnect(); err != nil {
				cytxApi.logger.Error("error reconnecting to cryptixd, waiting before retry: ", err)
				time.Sleep(5 * time.Second)
			}
		}
		select {
		case <-ctx.Done():
			cytxApi.logger.Warn("context cancelled, stopping block update listener")
			return
		case <-blockReadyChan:
			blockReadyCb()
			ticker.Reset(cytxApi.blockWaitTime)
		case <-ticker.C: // timeout, manually check for new blocks
			blockReadyCb()
		}
	}
}

func (cytxApi *CryptixApi) GetBlockTemplate(
	client *gostratum.StratumContext) (*appmessage.GetBlockTemplateResponseMessage, error) {
	template, err := cytxApi.cryptixd.GetBlockTemplate(client.WalletAddr,
		fmt.Sprintf(`'%s' via cryptix-network/cryptix-stratum-bridge_%s`, client.RemoteApp, version))
	if err != nil {
		return nil, errors.Wrap(err, "failed fetching new block template from cryptix")
	}
	return template, nil
}
