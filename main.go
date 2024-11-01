package main

import (
	"context"
	"log"
	"sync"

	"github.com/elnosh/gonuts/wallet"
	"github.com/lightningnetwork/lnd/lnrpc"
)

var pendingRebalanceAttempts *PendingRebalanceAttempts

func main() {
	env, err := loadEnv()
	if err != nil {
		log.Fatal(err)
	}

	lndConfig := LndConfig{
		GrpcHost:     env.grpcHost,
		TlsCertPath:  env.tlsCertPath,
		MacaroonPath: env.macaroonPath,
	}

	lndClient, err := SetupLndClient(lndConfig)
	if err != nil {
		log.Fatalf("error setting lnd client: %v", err)
	}

	walletConfig := wallet.Config{
		WalletPath:     env.walletPath,
		CurrentMintURL: env.mint,
	}
	cashuWallet, err := wallet.LoadWallet(walletConfig)
	if err != nil {
		log.Fatalf("could not load cashu wallet: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pendingRebalanceAttempts = NewPendingRebalanceAttempts()
	rebalanceAttemptChan := make(chan *rebalanceAttempt)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()

		htlcInterceptor, err := lndClient.routerClient.HtlcInterceptor(ctx)
		if err != nil {
			log.Fatal(err)
		}

		runInterceptor(ctx, htlcInterceptor, lndClient, rebalanceAttemptChan)
	}()

	go func() {
		defer wg.Done()
		subMessagesRequest := lnrpc.SubscribeCustomMessagesRequest{}
		subClient, err := lndClient.grpcClient.SubscribeCustomMessages(ctx, &subMessagesRequest)
		if err != nil {
			log.Fatal(err)
		}

		processRebalanceAttempts(ctx, subClient, lndClient, cashuWallet, rebalanceAttemptChan)
	}()

	wg.Wait()
}
