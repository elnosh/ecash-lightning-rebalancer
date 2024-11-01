package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strconv"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/elnosh/gonuts/cashu"
	"github.com/elnosh/gonuts/cashu/nuts/nut11"
	"github.com/elnosh/gonuts/wallet"
	"github.com/joho/godotenv"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	decodepay "github.com/nbd-wtf/ln-decodepay"
)

type Env struct {
	grpcHost     string
	tlsCertPath  string
	macaroonPath string
	walletPath   string
	mint         string
}

func loadEnv() (*Env, error) {
	if err := godotenv.Load(); err != nil {
		return nil, fmt.Errorf("could not load .env file: %v", err)
	}

	host := os.Getenv("LND_GRPC_HOST")
	if host == "" {
		return nil, fmt.Errorf("LND_GRPC_HOST cannot be empty")
	}
	certPath := os.Getenv("LND_CERT_PATH")
	if certPath == "" {
		return nil, fmt.Errorf("LND_CERT_PATH cannot be empty")
	}
	macaroonPath := os.Getenv("LND_MACAROON_PATH")
	if macaroonPath == "" {
		return nil, fmt.Errorf("LND_MACAROON_PATH cannot be empty")
	}

	walletPath := os.Getenv("WALLET_PATH")
	if len(walletPath) == 0 {
		walletPath = "./.cashu-wallet"
	}

	mint := os.Getenv("MINT")
	if len(mint) == 0 {
		mint = "http://127.0.0.1:3338"
	}

	return &Env{
		grpcHost:     host,
		tlsCertPath:  certPath,
		macaroonPath: macaroonPath,
		walletPath:   walletPath,
		mint:         mint,
	}, nil
}

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

	htlcInterceptor, err := lndClient.routerClient.HtlcInterceptor(ctx)
	if err != nil {
		log.Fatal(err)
	}

	slog.Info("starting htlc interceptor")

	pendingRebalanceAttempts := NewPendingRebalanceAttempts()

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		for {
			htlcInterceptedRequest, err := htlcInterceptor.Recv()
			if err != nil {
				slog.Error(fmt.Sprintf("HTLC Interceptor Recv error: %v", err))
				continue
			}

			slog.Info("received htlc from htlc interceptor")

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

				if err := htlcInterceptor.Send(&interceptResponse); err != nil {
					slog.Error(fmt.Sprintf("could not send intercept response to interceptor: %v", err))
				}
				slog.Info("enough liquidity, resuming htlc")
				continue
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
		}
	}()

	go func() {
		subMessagesRequest := lnrpc.SubscribeCustomMessagesRequest{}
		subClient, err := lndClient.grpcClient.SubscribeCustomMessages(ctx, &subMessagesRequest)
		if err != nil {
			log.Fatalf("could not subscribe to custom peer messages: %v", err)
		}

		for {
			customMessageReceived, err := subClient.Recv()
			if err != nil {
				slog.Error(fmt.Sprintf("error receiving message from peer: %v", err))
				continue
			}

			switch customMessageReceived.Type {
			case REBALANCE_REQUEST:
				var rebalanceRequest RebalanceRequest
				if err := json.Unmarshal(customMessageReceived.Data, &rebalanceRequest); err != nil {
					slog.Error(fmt.Sprintf("could not parse rebalance request message: %v", err))
					continue
				}

				slog.Info(fmt.Sprintf("received request to rebalance channel for %v sats", rebalanceRequest.Amount))

				// TODO: add fees that node wants to charge
				ecashAmount := rebalanceRequest.Amount
				rebalanceResponse := RebalanceResponse{
					Id:          rebalanceRequest.Id,
					PublicKey:   cashuWallet.GetReceivePubkey().SerializeCompressed(),
					Mints:       cashuWallet.TrustedMints(),
					EcashAmount: ecashAmount,
				}

				peer := hex.EncodeToString(customMessageReceived.Peer)
				if err := lndClient.SendCustomMessage(ctx, rebalanceResponse, peer); err != nil {
					slog.Error(fmt.Sprintf("could not send rebalance response message: %v", err))
					continue
				}
				slog.Info(fmt.Sprintf("sent rebalance response for %v sats to peer", ecashAmount))

				rebalanceAttempt := rebalanceAttempt{
					id:          rebalanceRequest.Id,
					publicKey:   cashuWallet.GetReceivePubkey().SerializeCompressed(),
					mints:       cashuWallet.TrustedMints(),
					ecashAmount: ecashAmount,
				}
				pendingRebalanceAttempts.Put(rebalanceRequest.Id, rebalanceAttempt)

			case REBALANCE_RESPONSE:
				var rebalanceResponse RebalanceResponse
				if err := json.Unmarshal(customMessageReceived.Data, &rebalanceResponse); err != nil {
					slog.Error(fmt.Sprintf("could not parse rebalance response message: %v", err))
					continue
				}

				slog.Info(fmt.Sprintf("received rebalance response for %v sats", rebalanceResponse.EcashAmount))

				rebalance, ok := pendingRebalanceAttempts.Get(rebalanceResponse.Id)
				if !ok {
					logmsg := fmt.Sprintf("received 'REBALANCE_RESPONSE' message with unidentified id: %v ignoring...", rebalanceResponse.Id)
					slog.Info(logmsg)
					continue
				}

				// TODO: check willing to accept rebalanceResponse.EcashAmount

				publicKey, err := secp256k1.ParsePubKey(rebalanceResponse.PublicKey)
				if err != nil {
					slog.Error(fmt.Sprintf("got invalid public key in REBALANCE_RESPONSE message: %v", err))
					continue
				}

				preimage := make([]byte, 32)
				_, err = rand.Read(preimage)
				if err != nil {
					slog.Error(fmt.Sprintf("error generating random preimage: %v", err))
					continue
				}
				hexPreimage := hex.EncodeToString(preimage)

				addInvoiceRequest := lnrpc.Invoice{
					RPreimage: preimage,
					Value:     int64(rebalance.amount),
				}
				invoice, err := lndClient.grpcClient.AddInvoice(ctx, &addInvoiceRequest)
				if err != nil {
					slog.Error(fmt.Sprintf("error creating invoice for rebalance: %v", err))
					continue
				}

				// TODO: if don't have any proofs from specified trusted mints by peer do swap for trusted mint

				tags := nut11.P2PKTags{
					NSigs:   1,
					Pubkeys: []*btcec.PublicKey{publicKey},
					// TODO: add locktime
				}
				htlcLockedProofs, err := cashuWallet.HTLCLockedProofs(
					rebalanceResponse.EcashAmount,
					cashuWallet.CurrentMint(),
					hexPreimage,
					&tags,
					true,
				)
				if err != nil {
					slog.Error(fmt.Sprintf("error generating ecash HTLC: %v", err))
					continue
				}

				ecashHTLC, err := cashu.NewTokenV4(htlcLockedProofs, cashuWallet.CurrentMint(), cashu.Sat, false)
				if err != nil {
					slog.Error(fmt.Sprintf("error creating ecash HTLC: %v", err))
					continue
				}
				cashuToken, err := ecashHTLC.Serialize()
				if err != nil {
					slog.Error(fmt.Sprintf("error creating ecash HTLC: %v", err))
					continue
				}

				htlcSwapMessage := HTLCSwap{
					Id:        rebalance.id,
					Invoice:   invoice.PaymentRequest,
					EcashHTLC: cashuToken,
				}
				peer := hex.EncodeToString(customMessageReceived.Peer)
				if err := lndClient.SendCustomMessage(ctx, htlcSwapMessage, peer); err != nil {
					slog.Error(fmt.Sprintf("could not send HTLC Swap message: %v", err))
					continue
				}
				slog.Info("sent HTLC Swap message to peer")

				rebalance.publicKey = rebalanceResponse.PublicKey
				rebalance.mints = rebalanceResponse.Mints
				rebalance.ecashAmount = rebalanceResponse.EcashAmount
				rebalance.invoice = invoice.PaymentRequest
				rebalance.ecashHTLC = cashuToken
				pendingRebalanceAttempts.Put(rebalance.id, rebalance)

				// subscribe to invoice changes. If paid, resume intercepted htlc
				go func() {
					subscribeInvoice := invoicesrpc.SubscribeSingleInvoiceRequest{
						RHash: invoice.RHash,
					}

					invoiceSubClient, err := lndClient.invoicesClient.SubscribeSingleInvoice(ctx, &subscribeInvoice)
					if err != nil {
						slog.Error(fmt.Sprintf("error setting invoice subscription: %v", err))
					}

					for {
						invoiceChange, err := invoiceSubClient.Recv()
						if err != nil {
							slog.Error("could not receive invoice state change")
							continue
						}

						// if invoice from rebalance request was paid, resume previously held HTLC since it should have
						// enough liquidity now to route payment
						if invoiceChange.State == lnrpc.Invoice_SETTLED {
							logmsg := fmt.Sprintf("invoice %x for rebalancing channel was paid. Resuming intercepted HTLC", invoice.RHash)
							slog.Info(logmsg)

							htlcInterceptResponse := routerrpc.ForwardHtlcInterceptResponse{
								IncomingCircuitKey: rebalance.circuitKey,
								Action:             routerrpc.ResolveHoldForwardAction_RESUME,
							}

							if err := htlcInterceptor.Send(&htlcInterceptResponse); err != nil {
								slog.Error("could not send resolve action for intercepted htlc")
							}
							break
						}
					}
				}()

			case HTLC_SWAP:
				var swapMsg HTLCSwap
				if err := json.Unmarshal(customMessageReceived.Data, &swapMsg); err != nil {
					slog.Error(fmt.Sprintf("could not parse HTLC swap message: %v", err))
					continue
				}

				slog.Info("received HTLC swap message")

				rebalance, ok := pendingRebalanceAttempts.Get(swapMsg.Id)
				if !ok {
					logmsg := fmt.Sprintf("received 'HTLC_SWAP' message with unidentified id: %v ignoring...", swapMsg.Id)
					slog.Info(logmsg)
					continue
				}

				bolt11, err := decodepay.Decodepay(swapMsg.Invoice)
				if err != nil {
					slog.Error(fmt.Sprintf("could not decode invoice in swap message: %v", err))
					continue
				}
				slog.Info(fmt.Sprintf("invoice is for '%v' sats", bolt11.MSatoshi/1000))

				token, err := cashu.DecodeToken(swapMsg.EcashHTLC)
				if err != nil {
					slog.Error(fmt.Sprintf("could not decode cashu token in swap message: %v", err))
					continue
				}

				// TODO: check ecash comes from one of specified trusted mints
				proofs := token.Proofs()
				if err = VerifyEcashHTLC(proofs, bolt11, cashuWallet.GetReceivePubkey(), rebalance.ecashAmount); err != nil {
					slog.Error(err.Error())
					continue
				}

				// if ecash verification passed, pay invoice and redeem ecash
				sendPaymentReq := lnrpc.SendRequest{PaymentRequest: swapMsg.Invoice}
				sendPaymentRes, err := lndClient.grpcClient.SendPaymentSync(ctx, &sendPaymentReq)
				if err != nil {
					slog.Error(fmt.Sprintf("error paying invoice: %v", err))
					continue
				}

				if len(sendPaymentRes.PaymentError) > 0 {
					slog.Error(fmt.Sprintf("error paying invoice: %v", err))
					continue
				}

				preimage := hex.EncodeToString(sendPaymentRes.PaymentPreimage)
				ecashAmountReceived, err := cashuWallet.ReceiveHTLC(token, preimage)
				if err != nil {
					slog.Error(fmt.Sprintf("something bad happened, could not unlock redeem ecash HTLC: %v", err))
					continue
				}

				slog.Info(fmt.Sprintf("paid invoice '%v' and got %v sats in ecash", swapMsg.Invoice, ecashAmountReceived))

				pendingRebalanceAttempts.Delete(rebalance.id)
			default:
				// ignore
				continue
			}

		}
	}()

	wg.Wait()
}
