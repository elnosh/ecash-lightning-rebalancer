package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
)

func runInterceptor(
	ctx context.Context,
	htlcInterceptor routerrpc.Router_HtlcInterceptorClient,
	lndClient *LndClient,
	rebalanceChan chan *rebalanceAttempt,
) {
	slog.Info("starting htlc interceptor")

	go func() {
		for {
			htlcInterceptedRequest, err := htlcInterceptor.Recv()
			if err != nil {
				slog.Error(fmt.Sprintf("HTLC Interceptor Recv error: %v", err))
				continue
			}
			slog.Info("received HTLC from interceptor")

			channel, err := lndClient.GetChannelById(ctx, htlcInterceptedRequest.OutgoingRequestedChanId)
			if err != nil {
				slog.Error(fmt.Sprintf("could not get channel by id: %v", err))
				continue
			}

			outgoingAmountSat := htlcInterceptedRequest.OutgoingAmountMsat / 1000
			slog.Info(fmt.Sprintf("local balance: %v sats", channel.LocalBalance))
			slog.Info(fmt.Sprintf("outgoing amount: %v sats", outgoingAmountSat))

			if channel.LocalBalance > int64(outgoingAmountSat) {
				// if there is enough liquidity, resume payment
				interceptResponse := routerrpc.ForwardHtlcInterceptResponse{
					IncomingCircuitKey: htlcInterceptedRequest.IncomingCircuitKey,
					Action:             routerrpc.ResolveHoldForwardAction_RESUME,
				}
				slog.Info("enough liquidity, resuming htlc")

				if err := htlcInterceptor.Send(&interceptResponse); err != nil {
					slog.Error(fmt.Sprintf("could not resume intercepted HTLC: %v", err))
					continue
				}
			}

			neededLiquidity := outgoingAmountSat

			slog.Info(fmt.Sprintf("not enough liquidity to route htlc. requesting rebalance for %v sats with ecash to peer", neededLiquidity))
			circuitKey := htlcInterceptedRequest.IncomingCircuitKey
			id := strconv.FormatUint(circuitKey.ChanId, 10) + strconv.FormatUint(circuitKey.HtlcId, 10)

			rebalanceRequest := RebalanceRequest{
				Id:     id,
				Amount: neededLiquidity,
			}
			if err := lndClient.SendCustomMessage(ctx, rebalanceRequest, channel.RemotePubkey); err != nil {
				slog.Error(fmt.Sprintf("could not send rebalance request message: %v", err))
				continue
			}

			rebalanceAttempt := rebalanceAttempt{
				circuitKey: circuitKey,
				id:         id,
				amount:     neededLiquidity,
			}
			pendingRebalanceAttempts.Put(id, rebalanceAttempt)

			// listen to rebalance chan and resume HTLC
			go func() {
				rebalance := <-rebalanceChan
				// resume previously held HTLC
				htlcInterceptResponse := routerrpc.ForwardHtlcInterceptResponse{
					IncomingCircuitKey: rebalance.circuitKey,
					Action:             routerrpc.ResolveHoldForwardAction_RESUME,
				}

				if err := htlcInterceptor.Send(&htlcInterceptResponse); err != nil {
					slog.Error("could not send resolve action for intercepted htlc")
				}
				pendingRebalanceAttempts.Delete(rebalance.id)
			}()
		}
	}()
}
